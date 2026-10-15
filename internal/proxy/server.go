package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"go-tcp-proxy/internal/config"
	"go-tcp-proxy/internal/metrics"
)

type Server struct {
	route  config.Route
	log    *slog.Logger
	lim    *limiter
	dialer net.Dialer

	activeGauge                   prometheus.Gauge
	dialDuration, sessionDuration prometheus.Observer
	accepted, dialErrors          prometheus.Counter
	bytesUp, bytesDown            prometheus.Counter
	rejected, closed              *prometheus.CounterVec

	ln     net.Listener
	ready  atomic.Bool
	nextID atomic.Uint64

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	closing bool
	wg      sync.WaitGroup
}

func NewServer(route config.Route, log *slog.Logger, m *metrics.Metrics) *Server {
	name := route.Name
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		route: route,
		log:   log.With("route", name),
		lim:   newLimiter(route.Limits.MaxConnections, route.Limits.MaxConnectionsPerIP),
		dialer: net.Dialer{
			Timeout:   route.Timeouts.Dial,
			KeepAlive: keepAlive(route.TCP.KeepAlive),
		},
		activeGauge:     m.ConnectionsActive.WithLabelValues(name),
		dialDuration:    m.UpstreamDialDuration.WithLabelValues(name),
		sessionDuration: m.SessionDuration.WithLabelValues(name),
		accepted:        m.ConnectionsAccepted.WithLabelValues(name),
		dialErrors:      m.UpstreamDialErrors.WithLabelValues(name),
		bytesUp:         m.BytesTransferred.WithLabelValues(name, metrics.DirClientToUpstream),
		bytesDown:       m.BytesTransferred.WithLabelValues(name, metrics.DirUpstreamToClient),
		rejected:        m.ConnectionsRejected.MustCurryWith(prometheus.Labels{"route": name}),
		closed:          m.SessionsClosed.MustCurryWith(prometheus.Labels{"route": name}),
		ctx:             ctx,
		cancel:          cancel,
	}
	m.ConnectionsMax.WithLabelValues(name).Set(float64(route.Limits.MaxConnections))
	return s
}

func keepAlive(d time.Duration) time.Duration {
	if d == 0 {
		return -1
	}
	return d
}

func (s *Server) Listen() error {
	lc := net.ListenConfig{KeepAlive: keepAlive(s.route.TCP.KeepAlive)}
	ln, err := lc.Listen(context.Background(), "tcp", s.route.Listen)
	if err != nil {
		return fmt.Errorf("route %q: listen %s: %w", s.route.Name, s.route.Listen, err)
	}
	s.ln = ln
	return nil
}

func (s *Server) Addr() net.Addr { return s.ln.Addr() }

func (s *Server) Ready() bool { return s.ready.Load() }

func (s *Server) Serve() error {
	s.ready.Store(true)
	defer s.ready.Store(false)
	s.log.Info("route started", "listen", s.ln.Addr().String(), "upstream", s.route.Upstream)

	var backoff time.Duration
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}

			backoff = min(max(backoff*2, 5*time.Millisecond), time.Second)
			s.log.Warn("accept failed", "err", err, "retry_in", backoff)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	ip := clientIP(conn)
	if reason, ok := s.lim.tryAcquire(ip); !ok {
		s.reject(conn, ip, reason)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		s.lim.release(ip)
		s.reject(conn, ip, rejectClosed)
		return
	}
	s.wg.Go(func() {
		defer s.lim.release(ip)
		s.serveSession(conn, ip)
	})
}

func (s *Server) reject(conn net.Conn, ip netip.Addr, reason string) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = conn.Close()
	s.rejected.WithLabelValues(reason).Inc()
	s.log.Debug("connection rejected", "client", ip, "reason", reason)
}

func (s *Server) serveSession(client net.Conn, ip netip.Addr) {
	start := time.Now()
	s.accepted.Inc()
	s.activeGauge.Inc()
	defer s.activeGauge.Dec()

	log := s.log.With("conn_id", s.nextID.Add(1), "client", client.RemoteAddr().String())

	upstream, err := s.dialer.DialContext(s.ctx, "tcp", s.route.Upstream)
	dialTime := time.Since(start)
	s.dialDuration.Observe(dialTime.Seconds())
	if err != nil {
		_ = client.Close()
		s.dialErrors.Inc()
		s.closed.WithLabelValues(reasonDialError).Inc()
		log.Warn("upstream dial failed", "upstream", s.route.Upstream, "err", err)
		return
	}
	log.Info("connection established",
		"upstream", upstream.RemoteAddr().String(),
		"upstream_local", upstream.LocalAddr().String(),
		"dial_time", dialTime.String(),
		"active", s.lim.active())

	if !s.route.TCP.NoDelay {
		for _, c := range []net.Conn{client, upstream} {
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(false)
			}
		}
	}

	sess := newSession(client, upstream, s.route.Timeouts, log)
	sess.run(s.ctx, s.bytesUp, s.bytesDown)

	reason := sess.closeReason()
	s.closed.WithLabelValues(reason).Inc()
	s.sessionDuration.Observe(time.Since(start).Seconds())
	log.Info("connection closed",
		"reason", reason,
		"duration", time.Since(start).String(),
		"bytes_up", sess.bytesUp.Load(),
		"bytes_down", sess.bytesDown.Load())
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.ready.Store(false)
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	if s.ln != nil {
		_ = s.ln.Close()
	}
	s.log.Info("draining sessions", "active", s.lim.active())

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		s.cancel()
		return nil
	case <-ctx.Done():
	}

	n := s.lim.active()
	s.cancel()
	s.log.Warn("drain timeout, closed sessions forcibly", "closed", n)
	<-done
	return ctx.Err()
}

func clientIP(c net.Conn) netip.Addr {
	if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		return ta.AddrPort().Addr().Unmap()
	}
	return netip.Addr{}
}
