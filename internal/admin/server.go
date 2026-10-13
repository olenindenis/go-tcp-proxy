package admin

import (
	"context"
	"net"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Server struct {
	addr string
	app  *fiber.App
}

func New(addr string, reg *prometheus.Registry, ready func() bool) *Server {
	app := fiber.New(fiber.Config{
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	})
	app.Get("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	app.Get("/healthz", func(c fiber.Ctx) error {
		return c.SendString("ok\n")
	})
	app.Get("/readyz", func(c fiber.Ctx) error {
		if !ready() {
			return c.Status(fiber.StatusServiceUnavailable).SendString("not ready\n")
		}
		return c.SendString("ready\n")
	})
	return &Server{addr: addr, app: app}
}

func (s *Server) Listen() (net.Listener, error) {
	return net.Listen("tcp", s.addr)
}

func (s *Server) Serve(ln net.Listener) error {
	return s.app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.app.ShutdownWithContext(ctx)
}
