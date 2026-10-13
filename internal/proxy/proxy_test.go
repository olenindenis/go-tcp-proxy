package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"go-tcp-proxy/internal/config"
	"go-tcp-proxy/internal/metrics"
)

func startUpstream(t *testing.T, handler func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() {
					_ = c.Close()
				}()
				handler(c)
			}()
		}
	}()
	return ln.Addr().String()
}

func echo(c net.Conn) {
	_, _ = io.Copy(c, c)
	_ = c.(*net.TCPConn).CloseWrite()
}

type testProxy struct {
	*Server
	m *metrics.Metrics
}

func startProxy(t *testing.T, upstream string, tweak func(*config.Route)) *testProxy {
	t.Helper()
	cfg := config.Default()
	cfg.AddRoute("test", "127.0.0.1:0", upstream)
	r := &cfg.Routes[0]
	r.Timeouts.Idle = 2 * time.Second
	r.Timeouts.HalfClose = 2 * time.Second
	r.Timeouts.Dial = time.Second
	if tweak != nil {
		tweak(r)
	}
	m := metrics.New()
	s := NewServer(*r, slog.New(slog.DiscardHandler), m)
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return &testProxy{Server: s, m: m}
}

func (p *testProxy) dial(t *testing.T) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func (p *testProxy) closed(reason string) float64 {
	return testutil.ToFloat64(p.m.SessionsClosed.WithLabelValues("test", reason))
}

func (p *testProxy) rejected(reason string) float64 {
	return testutil.ToFloat64(p.m.ConnectionsRejected.WithLabelValues("test", reason))
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func roundTrip(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != msg {
		t.Fatalf("got %q, want %q", buf, msg)
	}
}

func expectClosed(t *testing.T, c net.Conn, within time.Duration) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(within))
	_, err := io.Copy(io.Discard, c)
	if isTimeout(err) {
		t.Fatalf("connection still open after %v", within)
	}
}

func (p *testProxy) expectRejected(t *testing.T) {
	t.Helper()
	c, err := net.DialTimeout("tcp", p.Addr().String(), time.Second)
	if err != nil {
		return
	}
	defer func() {
		_ = c.Close()
	}()
	expectClosed(t, c, time.Second)
}

func TestForwardAndHalfClose(t *testing.T) {
	p := startProxy(t, startUpstream(t, echo), nil)
	c := p.dial(t)

	roundTrip(t, c, "ping")
	payload := bytes.Repeat([]byte("x"), 1<<20)
	go func() {
		_, _ = c.Write(payload)
		_ = c.(*net.TCPConn).CloseWrite()
	}()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payload) {
		t.Fatalf("echoed %d bytes, want %d", len(got), len(payload))
	}

	eventually(t, "eof close", func() bool { return p.closed(reasonEOF) == 1 })
	want := float64(len(payload) + 4)
	for _, dir := range []string{metrics.DirClientToUpstream, metrics.DirUpstreamToClient} {
		if got := testutil.ToFloat64(p.m.BytesTransferred.WithLabelValues("test", dir)); got != want {
			t.Errorf("bytes %s = %v, want %v", dir, got, want)
		}
	}
	if got := testutil.ToFloat64(p.m.ConnectionsActive.WithLabelValues("test")); got != 0 {
		t.Errorf("active = %v", got)
	}
}

func TestGlobalLimit(t *testing.T) {
	p := startProxy(t, startUpstream(t, echo), func(r *config.Route) { r.Limits.MaxConnections = 1 })
	first := p.dial(t)
	roundTrip(t, first, "a")

	p.expectRejected(t)
	eventually(t, "global rejection", func() bool { return p.rejected(rejectGlobal) == 1 })

	_ = first.Close()
	eventually(t, "slot release", func() bool { return p.lim.active() == 0 })
	roundTrip(t, p.dial(t), "b")
}

func TestPerIPLimit(t *testing.T) {
	p := startProxy(t, startUpstream(t, echo), func(r *config.Route) { r.Limits.MaxConnectionsPerIP = 2 })
	roundTrip(t, p.dial(t), "a")
	roundTrip(t, p.dial(t), "b")
	p.expectRejected(t)
	eventually(t, "per-ip rejection", func() bool { return p.rejected(rejectPerIP) == 1 })
}

func TestIdleTimeout(t *testing.T) {
	p := startProxy(t, startUpstream(t, echo), func(r *config.Route) { r.Timeouts.Idle = 200 * time.Millisecond })
	c := p.dial(t)
	roundTrip(t, c, "a")
	start := time.Now()
	expectClosed(t, c, 2*time.Second)
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("closed too early: %v", d)
	}
	eventually(t, "idle close", func() bool { return p.closed(reasonIdleTimeout) == 1 })
}

func TestIdleIsPerSession(t *testing.T) {
	upstream := startUpstream(t, func(c net.Conn) {
		for range 8 {
			time.Sleep(50 * time.Millisecond)
			if _, err := c.Write([]byte("tick")); err != nil {
				return
			}
		}
	})
	p := startProxy(t, upstream, func(r *config.Route) { r.Timeouts.Idle = 200 * time.Millisecond })
	c := p.dial(t)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, _ := io.ReadAll(c)
	if len(got) != 8*4 {
		t.Fatalf("received %d bytes, want 32", len(got))
	}
	if p.closed(reasonIdleTimeout) != 0 {
		t.Fatal("session closed by idle timeout despite upstream activity")
	}
}

func TestHalfCloseTimeout(t *testing.T) {

	upstream := startUpstream(t, func(c net.Conn) { _, _ = io.Copy(io.Discard, c); select {} })
	p := startProxy(t, upstream, func(r *config.Route) { r.Timeouts.HalfClose = 200 * time.Millisecond })
	c := p.dial(t)
	_ = c.(*net.TCPConn).CloseWrite()
	expectClosed(t, c, 2*time.Second)
	eventually(t, "half-close timeout", func() bool { return p.closed(reasonHalfClose) == 1 })
}

func TestMaxSession(t *testing.T) {
	p := startProxy(t, startUpstream(t, echo), func(r *config.Route) { r.Timeouts.MaxSession = 300 * time.Millisecond })
	c := p.dial(t)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := c.Write([]byte("x")); err != nil {
			break
		}
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := c.Read(make([]byte, 1)); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("active session was not closed by max_session")
		}
		time.Sleep(20 * time.Millisecond)
	}
	eventually(t, "max lifetime close", func() bool { return p.closed(reasonMaxLifetime) == 1 })
}

func TestDialError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	p := startProxy(t, addr, nil)
	expectClosed(t, p.dial(t), 2*time.Second)
	eventually(t, "dial error", func() bool {
		return testutil.ToFloat64(p.m.UpstreamDialErrors.WithLabelValues("test")) == 1 &&
			p.closed(reasonDialError) == 1
	})
}

func TestGracefulShutdown(t *testing.T) {
	p := startProxy(t, startUpstream(t, echo), nil)
	c := p.dial(t)
	roundTrip(t, c, "a")

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		done <- p.Shutdown(ctx)
	}()
	eventually(t, "not ready", func() bool { return !p.Ready() })

	p.expectRejected(t)
	roundTrip(t, c, "still alive")

	_ = c.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish after the last session closed")
	}
}

func TestForcedShutdown(t *testing.T) {
	p := startProxy(t, startUpstream(t, echo), nil)
	c := p.dial(t)
	roundTrip(t, c, "a")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := p.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want deadline exceeded", err)
	}
	expectClosed(t, c, time.Second)
	if p.closed(reasonShutdown) != 1 {
		t.Fatalf("shutdown closes = %v", p.closed(reasonShutdown))
	}
}

func TestManagerMultipleRoutes(t *testing.T) {
	upA := startUpstream(t, func(c net.Conn) { _, _ = c.Write([]byte("A")) })
	upB := startUpstream(t, func(c net.Conn) { _, _ = c.Write([]byte("B")) })
	cfg := config.Default()
	cfg.AddRoute("a", "127.0.0.1:0", upA)
	cfg.AddRoute("b", "127.0.0.1:0", upB)
	mgr := NewManager(cfg, slog.New(slog.DiscardHandler), metrics.New())
	if err := mgr.Listen(); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- mgr.Serve() }()
	eventually(t, "ready", mgr.Ready)

	for i, want := range []string{"A", "B"} {
		c, err := net.Dial("tcp", mgr.Servers()[i].Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		got, _ := io.ReadAll(c)
		_ = c.Close()
		if string(got) != want {
			t.Errorf("route %d: got %q, want %q", i, got, want)
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := mgr.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if mgr.Ready() {
		t.Fatal("manager still ready after shutdown")
	}
}
