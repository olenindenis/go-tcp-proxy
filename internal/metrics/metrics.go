package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

const namespace = "tcp_proxy"

const (
	DirClientToUpstream = "client_to_upstream"
	DirUpstreamToClient = "upstream_to_client"
)

type Metrics struct {
	Registry *prometheus.Registry

	ConnectionsActive    *prometheus.GaugeVec
	ConnectionsMax       *prometheus.GaugeVec
	ConnectionsAccepted  *prometheus.CounterVec
	ConnectionsRejected  *prometheus.CounterVec
	UpstreamDialErrors   *prometheus.CounterVec
	UpstreamDialDuration *prometheus.HistogramVec
	SessionsClosed       *prometheus.CounterVec
	SessionDuration      *prometheus.HistogramVec
	BytesTransferred     *prometheus.CounterVec
}

func New() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		ConnectionsActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "connections_active",
			Help: "Number of currently active proxied sessions.",
		}, []string{"route"}),
		ConnectionsMax: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "connections_max",
			Help: "Configured maximum number of concurrent sessions.",
		}, []string{"route"}),
		ConnectionsAccepted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "connections_accepted_total",
			Help: "Client connections accepted within limits.",
		}, []string{"route"}),
		ConnectionsRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "connections_rejected_total",
			Help: "Client connections rejected because a limit was reached.",
		}, []string{"route", "reason"}),
		UpstreamDialErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "upstream_dial_errors_total",
			Help: "Failed attempts to connect to the upstream.",
		}, []string{"route"}),
		UpstreamDialDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "upstream_dial_duration_seconds",
			Help:    "Time spent establishing upstream connections.",
			Buckets: prometheus.ExponentialBuckets(0.0005, 2, 14),
		}, []string{"route"}),
		SessionsClosed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "sessions_closed_total",
			Help: "Closed sessions by close reason.",
		}, []string{"route", "reason"}),
		SessionDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "session_duration_seconds",
			Help:    "Lifetime of proxied sessions.",
			Buckets: []float64{0.1, 0.5, 1, 5, 10, 30, 60, 300, 600, 1800, 3600},
		}, []string{"route"}),
		BytesTransferred: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "bytes_transferred_total",
			Help: "Bytes forwarded by direction.",
		}, []string{"route", "direction"}),
	}
	m.Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.ConnectionsActive, m.ConnectionsMax, m.ConnectionsAccepted, m.ConnectionsRejected,
		m.UpstreamDialErrors, m.UpstreamDialDuration, m.SessionsClosed, m.SessionDuration,
		m.BytesTransferred,
	)
	return m
}
