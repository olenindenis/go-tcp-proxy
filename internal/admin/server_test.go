package admin

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/client"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"go-tcp-proxy/internal/metrics"
)

func TestEndpoints(t *testing.T) {
	m := metrics.New()
	m.ConnectionsActive.WithLabelValues("test").Set(3)
	var ready atomic.Bool
	s := New("127.0.0.1:0", m.Registry, ready.Load)
	ln, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	base := "http://" + ln.Addr().String()
	cc := client.New().SetTimeout(2 * time.Second)

	get := func(path string) int {
		t.Helper()
		resp, err := cc.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Close()
		return resp.StatusCode()
	}

	if code := get("/healthz"); code != fiber.StatusOK {
		t.Errorf("/healthz = %d", code)
	}
	if code := get("/readyz"); code != fiber.StatusServiceUnavailable {
		t.Errorf("/readyz before ready = %d", code)
	}
	ready.Store(true)
	if code := get("/readyz"); code != fiber.StatusOK {
		t.Errorf("/readyz after ready = %d", code)
	}
	const want = `
# HELP tcp_proxy_connections_active Number of currently active proxied sessions.
# TYPE tcp_proxy_connections_active gauge
tcp_proxy_connections_active{route="test"} 3
`
	if err := testutil.ScrapeAndCompare(base+"/metrics", strings.NewReader(want), "tcp_proxy_connections_active"); err != nil {
		t.Error(err)
	}
}
