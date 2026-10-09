// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

// Package contains the collection of prometheus meters/counters
// and related update methods
package metrics

import (
	"context"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ory/x/logrusx"
	"github.com/ory/x/prometheusx"

	"github.com/ory/oathkeeper/driver"
)

var (
	// RequestTotal provides the total number of requests
	RequestTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ory_oathkeeper_requests_total",
			Help: "Total number of requests",
		},
		[]string{"service", "method", "request", "status_code"},
	)
	// HistogramRequestDuration provides the duration of requests
	HistogramRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ory_oathkeeper_requests_duration_seconds",
			Help:    "Time spent serving requests.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"service", "method", "request", "status_code"},
	)
)

// RequestDurationObserve tracks request durations
type RequestDurationObserve func(ctx context.Context, histogram *prometheus.HistogramVec, service, request, method string, statusCode int) func(float64)

// UpdateRequest tracks total requests done
type UpdateRequest func(ctx context.Context, counter *prometheus.CounterVec, service, request, method string, statusCode int)

// PrometheusRepository provides methods to manage prometheus metrics
type PrometheusRepository struct {
	logger   *logrusx.Logger
	Registry *prometheus.Registry
	metrics  []prometheus.Collector
}

// NewConfigurablePrometheusRepository creates a new prometheus repository with the given settings
func NewConfigurablePrometheusRepository(d driver.Registry) *PrometheusRepository {
	RequestTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: d.Config().PrometheusMetricsNamePrefix() + "requests_total",
			Help: "Total number of requests",
		},
		[]string{"service", "method", "request", "status_code"},
	)
	HistogramRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    d.Config().PrometheusMetricsNamePrefix() + "requests_duration_seconds",
			Help:    "Time spent serving requests.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"service", "method", "request", "status_code"},
	)
	return NewPrometheusRepository(d.Logger())
}

// NewPrometheusRepository creates a new prometheus repository
func NewPrometheusRepository(logger *logrusx.Logger) *PrometheusRepository {
	m := []prometheus.Collector{
		prometheus.NewGoCollector(),                                       //nolint:staticcheck // compatible with current deps
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}), //nolint:staticcheck // compatible with current deps
		RequestTotal,
		HistogramRequestDuration,
	}

	r := prometheus.NewRegistry()

	for _, metric := range m {
		if err := r.Register(metric); err != nil {
			logger.WithError(err).Error("Unable to register prometheus metric.")
		}
	}

	mr := &PrometheusRepository{
		logger:   logger,
		Registry: r,
		metrics:  m,
	}

	return mr
}

// RequestDurationObserve tracks request durations. When ctx carries a
// sampled trace span, the observation carries a trace exemplar so dashboards
// can link latency outliers to traces.
func (r *PrometheusRepository) RequestDurationObserve(ctx context.Context, service, request, method string, statusCode int) func(float64) {
	observer := HistogramRequestDuration.With(prometheus.Labels{
		"service":     service,
		"method":      method,
		"request":     request,
		"status_code": strconv.Itoa(statusCode),
	})
	return func(v float64) {
		prometheusx.ObserveWithExemplar(ctx, observer, v)
	}
}

// UpdateRequest tracks total requests done. When ctx carries a sampled trace
// span, the increment carries a trace exemplar.
func (r *PrometheusRepository) UpdateRequest(ctx context.Context, service, request, method string, statusCode int) {
	prometheusx.AddWithExemplar(ctx, RequestTotal.With(prometheus.Labels{
		"service":     service,
		"method":      method,
		"request":     request,
		"status_code": strconv.Itoa(statusCode),
	}), 1)
}
