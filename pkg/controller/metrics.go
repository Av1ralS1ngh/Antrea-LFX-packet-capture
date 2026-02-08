package controller

import (
	"sync"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

var (
	// ActiveCaptures tracks the number of currently running captures
	ActiveCaptures = metrics.NewGauge(
		&metrics.GaugeOpts{
			Name:           "antrea_packet_capture_active_captures",
			Help:           "Number of currently active packet captures",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// CaptureStartsTotal tracks the total number of capture start attempts
	CaptureStartsTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Name:           "antrea_packet_capture_starts_total",
			Help:           "Total number of packet capture start attempts",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"result"}, // success, failure
	)
)

var registerMetricsOnce sync.Once

// RegisterMetrics registers the controller's metrics with the legacy registry
func RegisterMetrics() {
	registerMetricsOnce.Do(func() {
		legacyregistry.MustRegister(ActiveCaptures)
		legacyregistry.MustRegister(CaptureStartsTotal)
	})
}

// RecordCaptureStart records a capture start attempt
func RecordCaptureStart(err error) {
	result := "success"
	if err != nil {
		result = "failure"
	}
	CaptureStartsTotal.WithLabelValues(result).Inc()
}

// RecordCaptureActive updates the active capture gauge
func RecordCaptureActive(delta float64) {
	ActiveCaptures.Add(delta)
}
