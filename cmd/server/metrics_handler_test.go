// Copyright © 2026 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ory/oathkeeper/metrics"
	"github.com/ory/x/logrusx"
)

// The OpenMetrics exposition format is the only one that carries exemplars,
// so the metrics server must serve it when the scraper negotiates it.
func TestPrometheusHandlerServesOpenMetrics(t *testing.T) {
	prom := metrics.NewPrometheusRepository(logrusx.New("test", "test"))
	ts := httptest.NewServer(prometheusHandler(prom))
	t.Cleanup(ts.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "application/openmetrics-text;version=1.0.0")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/openmetrics-text")
}
