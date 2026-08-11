package observability

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics holds Relata's application metrics.
//
// These describe the health of the *service* — request rates, job outcomes,
// provider latency. They deliberately contain no stored-knowledge cardinality:
// no item IDs, no entity names, no query strings ever become label values.
type Metrics struct {
	Registry *prometheus.Registry

	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec
	HTTPInFlight prometheus.Gauge

	JobsProcessed *prometheus.CounterVec
	JobDuration   *prometheus.HistogramVec
	JobsQueued    *prometheus.GaugeVec

	AIRequests *prometheus.CounterVec
	AIDuration *prometheus.HistogramVec

	StorageOps      *prometheus.CounterVec
	StorageDuration *prometheus.HistogramVec

	IngestItems   *prometheus.CounterVec
	ModelInvalid  *prometheus.CounterVec
	AuthAttempts  *prometheus.CounterVec
	SearchOps     *prometheus.CounterVec
	SearchLatency prometheus.Histogram
}

var (
	metricsOnce sync.Once
	metricsInst *Metrics
)

// NewMetrics returns the process-wide metrics registry, creating it once.
func NewMetrics() *Metrics {
	metricsOnce.Do(func() {
		reg := prometheus.NewRegistry()
		reg.MustRegister(collectors.NewGoCollector())
		reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

		m := &Metrics{Registry: reg}

		m.HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relata_http_requests_total",
			Help: "HTTP requests by route pattern, method and status class.",
		}, []string{"route", "method", "status"})

		m.HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "relata_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds.",
			Buckets: []float64{0.005, 0.025, 0.1, 0.25, 1, 2.5, 10, 30},
		}, []string{"route", "method"})

		m.HTTPInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "relata_http_in_flight_requests",
			Help: "HTTP requests currently being served.",
		})

		m.JobsProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relata_jobs_processed_total",
			Help: "Background jobs finished, by type and outcome.",
		}, []string{"type", "outcome"})

		m.JobDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "relata_job_duration_seconds",
			Help:    "Background job execution duration in seconds.",
			Buckets: []float64{0.05, 0.25, 1, 5, 15, 60, 180, 600},
		}, []string{"type"})

		m.JobsQueued = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "relata_jobs_queued",
			Help: "Jobs currently in each status.",
		}, []string{"status"})

		m.AIRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relata_ai_requests_total",
			Help: "AI provider calls by provider, operation and outcome.",
		}, []string{"provider", "operation", "outcome"})

		m.AIDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "relata_ai_request_duration_seconds",
			Help:    "AI provider call duration in seconds.",
			Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 15, 60, 180},
		}, []string{"provider", "operation"})

		m.StorageOps = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relata_storage_operations_total",
			Help: "Storage operations by store, operation and outcome.",
		}, []string{"store", "operation", "outcome"})

		m.StorageDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "relata_storage_operation_duration_seconds",
			Help:    "Storage operation duration in seconds.",
			Buckets: []float64{0.001, 0.005, 0.025, 0.1, 0.5, 2, 10},
		}, []string{"store", "operation"})

		m.IngestItems = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relata_ingest_items_total",
			Help: "Items accepted for ingestion by source type and outcome.",
		}, []string{"source_type", "outcome"})

		m.ModelInvalid = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relata_model_invalid_output_total",
			Help: "Model responses rejected by schema validation, by task and reason.",
		}, []string{"task", "reason"})

		m.AuthAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relata_auth_attempts_total",
			Help: "Authentication attempts by outcome.",
		}, []string{"outcome"})

		m.SearchOps = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relata_search_operations_total",
			Help: "Search operations by mode and outcome.",
		}, []string{"mode", "outcome"})

		m.SearchLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "relata_search_duration_seconds",
			Help:    "End-to-end search duration in seconds.",
			Buckets: []float64{0.01, 0.05, 0.15, 0.5, 1.5, 5, 20},
		})

		reg.MustRegister(
			m.HTTPRequests, m.HTTPDuration, m.HTTPInFlight,
			m.JobsProcessed, m.JobDuration, m.JobsQueued,
			m.AIRequests, m.AIDuration,
			m.StorageOps, m.StorageDuration,
			m.IngestItems, m.ModelInvalid, m.AuthAttempts,
			m.SearchOps, m.SearchLatency,
		)
		metricsInst = m
	})
	return metricsInst
}
