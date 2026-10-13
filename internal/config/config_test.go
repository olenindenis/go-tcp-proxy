package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sample = `
admin_listen: ":9191"
defaults:
  limits:
    max_connections: 50
  timeouts:
    idle: 10m
routes:
  - name: mysql
    listen: ":3307"
    upstream: "mysql:3306"
  - name: redis-cache
    listen: ":6380"
    upstream: "redis:6379"
    limits:
      max_connections_per_ip: 5
    timeouts:
      idle: 1h
`

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "proxy.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoadInheritsDefaults(t *testing.T) {
	cfg, err := Load(writeFile(t, sample), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.AdminListen != ":9191" {
		t.Errorf("admin_listen = %q", cfg.AdminListen)
	}
	if len(cfg.Routes) != 2 {
		t.Fatalf("routes = %d", len(cfg.Routes))
	}
	mysql, redis := cfg.Routes[0], cfg.Routes[1]

	if mysql.Limits.MaxConnections != 50 || mysql.Timeouts.Idle != 10*time.Minute {
		t.Errorf("mysql did not inherit defaults: %+v", mysql.Settings)
	}

	if mysql.Timeouts.Dial != 5*time.Second || !mysql.TCP.NoDelay {
		t.Errorf("mysql lost built-in defaults: %+v", mysql.Settings)
	}
	if redis.Timeouts.Idle != time.Hour || redis.Limits.MaxConnectionsPerIP != 5 {
		t.Errorf("redis overrides not applied: %+v", redis.Settings)
	}
	if redis.Limits.MaxConnections != 50 || redis.Timeouts.HalfClose != 30*time.Second {
		t.Errorf("redis lost inherited values in partially overridden sections: %+v", redis.Settings)
	}
}

func TestEnvOverrides(t *testing.T) {
	cfg, err := Load(writeFile(t, sample), env(map[string]string{
		"PROXY_DEFAULTS_TIMEOUTS_IDLE":     "2m",
		"PROXY_DEFAULTS_TCP_NODELAY":       "false",
		"PROXY_ROUTE_REDIS_CACHE_UPSTREAM": "10.0.0.1:6379",
		"PROXY_SHUTDOWN_TIMEOUT":           "5s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Routes[0].Timeouts.Idle; got != 2*time.Minute {
		t.Errorf("env default not inherited by route: idle = %v", got)
	}
	if cfg.Routes[0].TCP.NoDelay {
		t.Error("nodelay override not applied")
	}
	if got := cfg.Routes[1].Upstream; got != "10.0.0.1:6379" {
		t.Errorf("route upstream = %q", got)
	}
	if cfg.ShutdownTimeout != 5*time.Second {
		t.Errorf("shutdown_timeout = %v", cfg.ShutdownTimeout)
	}
}

func TestEnvInvalidValue(t *testing.T) {
	_, err := Load("", env(map[string]string{"PROXY_DEFAULTS_LIMITS_MAX_CONNECTIONS": "lots"}))
	if err == nil || !strings.Contains(err.Error(), "PROXY_DEFAULTS_LIMITS_MAX_CONNECTIONS") {
		t.Fatalf("expected env error, got %v", err)
	}
}

func TestAddRouteWithoutFile(t *testing.T) {
	cfg, err := Load("", env(nil))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddRoute("default", ":8080", "localhost:80")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Routes[0].Timeouts.Idle != 15*time.Minute {
		t.Errorf("route did not inherit defaults")
	}
}

func TestValidate(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "no routes") {
		t.Errorf("expected 'no routes', got %v", err)
	}

	cfg.AddRoute("a", ":1000", "host:1")
	cfg.AddRoute("a", ":1000", "bad-addr")
	cfg.Routes[1].Timeouts.Idle = 0
	err := cfg.Validate()
	for _, want := range []string{"duplicate name", "duplicate listen", "upstream", "timeouts.idle"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("expected %q in %v", want, err)
		}
	}
}
