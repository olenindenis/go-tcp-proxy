package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const EnvPrefix = "PROXY_"

type Config struct {
	AdminListen     string        `yaml:"admin_listen"`
	LogLevel        string        `yaml:"log_level"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	Defaults        Settings      `yaml:"defaults"`
	Routes          []Route       `yaml:"-"`
}

type Route struct {
	Name     string `yaml:"name"`
	Listen   string `yaml:"listen"`
	Upstream string `yaml:"upstream"`
	Settings `yaml:",inline"`
}

type Settings struct {
	Limits   Limits   `yaml:"limits"`
	Timeouts Timeouts `yaml:"timeouts"`
	TCP      TCP      `yaml:"tcp"`
}

type Limits struct {
	MaxConnections      int `yaml:"max_connections"`
	MaxConnectionsPerIP int `yaml:"max_connections_per_ip"`
}

type Timeouts struct {
	Dial       time.Duration `yaml:"dial"`
	Idle       time.Duration `yaml:"idle"`
	MaxSession time.Duration `yaml:"max_session"`
	HalfClose  time.Duration `yaml:"half_close"`
}

type TCP struct {
	KeepAlive time.Duration `yaml:"keepalive"`
	NoDelay   bool          `yaml:"nodelay"`
}

func Default() *Config {
	return &Config{
		AdminListen:     ":9090",
		LogLevel:        "info",
		ShutdownTimeout: 30 * time.Second,
		Defaults: Settings{
			Limits: Limits{MaxConnections: 1000},
			Timeouts: Timeouts{
				Dial:      5 * time.Second,
				Idle:      15 * time.Minute,
				HalfClose: 30 * time.Second,
			},
			TCP: TCP{KeepAlive: 30 * time.Second, NoDelay: true},
		},
	}
}

type rawRoutes struct {
	Routes []yaml.Node `yaml:"routes"`
}

func Load(path string, lookup func(string) (string, bool)) (*Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	cfg := Default()
	var raw rawRoutes
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
		if len(doc.Content) > 0 {
			if err := errors.Join(doc.Decode(cfg), doc.Decode(&raw)); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", path, err)
			}
		}
	}

	if err := applyGlobalEnv(cfg, lookup); err != nil {
		return nil, err
	}
	for i, node := range raw.Routes {
		r := Route{Settings: cfg.Defaults}
		if err := node.Decode(&r); err != nil {
			return nil, fmt.Errorf("parse route #%d: %w", i, err)
		}
		cfg.Routes = append(cfg.Routes, r)
	}
	applyRouteEnv(cfg, lookup)
	return cfg, nil
}

func (c *Config) AddRoute(name, listen, upstream string) {
	c.Routes = append(c.Routes, Route{Name: name, Listen: listen, Upstream: upstream, Settings: c.Defaults})
}

func applyGlobalEnv(cfg *Config, lookup func(string) (string, bool)) error {
	d := &cfg.Defaults
	vars := map[string]any{
		"ADMIN_LISTEN":                           &cfg.AdminListen,
		"LOG_LEVEL":                              &cfg.LogLevel,
		"SHUTDOWN_TIMEOUT":                       &cfg.ShutdownTimeout,
		"DEFAULTS_LIMITS_MAX_CONNECTIONS":        &d.Limits.MaxConnections,
		"DEFAULTS_LIMITS_MAX_CONNECTIONS_PER_IP": &d.Limits.MaxConnectionsPerIP,
		"DEFAULTS_TIMEOUTS_DIAL":                 &d.Timeouts.Dial,
		"DEFAULTS_TIMEOUTS_IDLE":                 &d.Timeouts.Idle,
		"DEFAULTS_TIMEOUTS_MAX_SESSION":          &d.Timeouts.MaxSession,
		"DEFAULTS_TIMEOUTS_HALF_CLOSE":           &d.Timeouts.HalfClose,
		"DEFAULTS_TCP_KEEPALIVE":                 &d.TCP.KeepAlive,
		"DEFAULTS_TCP_NODELAY":                   &d.TCP.NoDelay,
	}
	var errs []error
	for key, dst := range vars {
		val, ok := lookup(EnvPrefix + key)
		if !ok {
			continue
		}
		if err := setValue(dst, val); err != nil {
			errs = append(errs, fmt.Errorf("%s%s: %w", EnvPrefix, key, err))
		}
	}
	return errors.Join(errs...)
}

func applyRouteEnv(cfg *Config, lookup func(string) (string, bool)) {
	for i := range cfg.Routes {
		r := &cfg.Routes[i]
		prefix := EnvPrefix + "ROUTE_" + envName(r.Name) + "_"
		if v, ok := lookup(prefix + "LISTEN"); ok {
			r.Listen = v
		}
		if v, ok := lookup(prefix + "UPSTREAM"); ok {
			r.Upstream = v
		}
	}
}

func envName(name string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, strings.ToUpper(name))
}

func setValue(dst any, val string) error {
	switch p := dst.(type) {
	case *string:
		*p = val
	case *int:
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		*p = n
	case *bool:
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		*p = b
	case *time.Duration:
		d, err := time.ParseDuration(val)
		if err != nil {
			return err
		}
		*p = d
	default:
		return fmt.Errorf("unsupported type %T", dst)
	}
	return nil
}

func (c *Config) Validate() error {
	var errs []error
	if len(c.Routes) == 0 {
		errs = append(errs, errors.New("no routes configured"))
	}
	if c.AdminListen != "" {
		if err := checkAddr(c.AdminListen); err != nil {
			errs = append(errs, fmt.Errorf("admin_listen: %w", err))
		}
	}
	if c.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("shutdown_timeout must be > 0"))
	}
	names := map[string]bool{}
	listens := map[string]bool{}
	for i, r := range c.Routes {
		where := fmt.Sprintf("route %q", r.Name)
		if r.Name == "" {
			where = fmt.Sprintf("route #%d", i)
			errs = append(errs, fmt.Errorf("%s: name is required", where))
		} else if names[r.Name] {
			errs = append(errs, fmt.Errorf("%s: duplicate name", where))
		}
		names[r.Name] = true
		if listens[r.Listen] {
			errs = append(errs, fmt.Errorf("%s: duplicate listen %q", where, r.Listen))
		}
		listens[r.Listen] = true
		if err := checkAddr(r.Listen); err != nil {
			errs = append(errs, fmt.Errorf("%s: listen: %w", where, err))
		}
		if err := checkAddr(r.Upstream); err != nil {
			errs = append(errs, fmt.Errorf("%s: upstream: %w", where, err))
		}
		errs = append(errs, r.validate(where)...)
	}
	return errors.Join(errs...)
}

func (s Settings) validate(where string) []error {
	var errs []error
	check := func(ok bool, msg string) {
		if !ok {
			errs = append(errs, fmt.Errorf("%s: %s", where, msg))
		}
	}
	check(s.Limits.MaxConnections > 0, "limits.max_connections must be > 0")
	check(s.Limits.MaxConnectionsPerIP >= 0, "limits.max_connections_per_ip must be >= 0")
	check(s.Timeouts.Dial > 0, "timeouts.dial must be > 0")
	check(s.Timeouts.Idle > 0, "timeouts.idle must be > 0")
	check(s.Timeouts.MaxSession >= 0, "timeouts.max_session must be >= 0")
	check(s.Timeouts.HalfClose > 0, "timeouts.half_close must be > 0")
	check(s.TCP.KeepAlive >= 0, "tcp.keepalive must be >= 0")
	return errs
}

func checkAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}
