package credentials

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the token exchange metrics of a replica.
type Metrics struct {
	// Exchanges counts exchanges by configured name, kind (start or
	// refresh) and outcome (success, refused or error).
	Exchanges *prometheus.CounterVec
}

// NewMetrics returns unregistered token exchange metrics.
func NewMetrics() *Metrics {
	return &Metrics{
		Exchanges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_token_exchanges_total",
			Help: "Token exchanges, by configured exchange, kind (start or refresh) and outcome (success, refused for an OAuth error answer, error otherwise).",
		}, []string{"exchange", "kind", "outcome"}),
	}
}

// Collectors returns the metrics to register.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.Exchanges}
}

func (m *Metrics) observe(exchange, kind, outcome string) {
	if m == nil {
		return
	}

	m.Exchanges.WithLabelValues(exchange, kind, outcome).Inc()
}
