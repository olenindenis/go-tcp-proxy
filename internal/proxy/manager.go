package proxy

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"go-tcp-proxy/internal/config"
	"go-tcp-proxy/internal/metrics"
)

type Manager struct {
	servers []*Server
}

func NewManager(cfg *config.Config, log *slog.Logger, m *metrics.Metrics) *Manager {
	mgr := &Manager{}
	for _, r := range cfg.Routes {
		mgr.servers = append(mgr.servers, NewServer(r, log, m))
	}
	return mgr
}

func (m *Manager) Servers() []*Server { return m.servers }

func (m *Manager) Listen() error {
	for i, s := range m.servers {
		if err := s.Listen(); err != nil {
			for _, bound := range m.servers[:i] {
				_ = bound.ln.Close()
			}
			return err
		}
	}
	return nil
}

func (m *Manager) Serve() error {
	errs := make(chan error, len(m.servers))
	for _, s := range m.servers {
		go func() { errs <- s.Serve() }()
	}
	for range m.servers {
		if err := <-errs; err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) Ready() bool {
	for _, s := range m.servers {
		if !s.Ready() {
			return false
		}
	}
	return len(m.servers) > 0
}

func (m *Manager) Shutdown(ctx context.Context) error {
	var wg sync.WaitGroup
	errs := make([]error, len(m.servers))
	for i, s := range m.servers {
		wg.Go(func() { errs[i] = s.Shutdown(ctx) })
	}
	wg.Wait()
	return errors.Join(errs...)
}
