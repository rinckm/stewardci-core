package tenantctl

import (
	"log"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics provides metrics
type Metrics interface {
	SetTenantNumber(float64)
	StartServer()
}

type metrics struct {
	TenantCount prometheus.Gauge
}

// NewMetrics create metrics
func NewMetrics() Metrics {
	return &metrics{
		TenantCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "steward_tenant_total_number",
			Help: "total number of tenants",
		}),
	}
}

// StartServer registers metrics and start http listener
func (metrics *metrics) StartServer() {
	prometheus.MustRegister(metrics.TenantCount)
	go provideMetrics()
}

func provideMetrics() {
	http.Handle("/metrics", promhttp.Handler())
	err := http.ListenAndServe(":9090", nil)
	if err != nil {
		log.Fatalf("Failed to start metrics server for tenant controller:%v", err)
	}
}

// SetTenantNumber sets the number of tenants
func (metrics *metrics) SetTenantNumber(count float64) {
	metrics.TenantCount.Set(count)
}
