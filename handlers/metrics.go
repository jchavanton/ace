// Copyright (C) 2026 Julien Chavanton <jchavanton@gmail.com>
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Named panels the /bots/:name/report page can request. Each template
// takes a single %s placeholder for the started_by filter (a PromQL
// label value pattern already quoted by the caller), which is how a
// bot's dashboard scopes itself to only that bot's runs.
//
// Kept as templates rather than pre-baked queries so a future
// `/scenarios/:name/report` page can reuse the same handler with a
// different filter shape.
var promPanels = map[string]string{
	// Runs per minute, split pass / fail / error. increase() over the
	// query step gives a stacked-area shape that reads naturally as
	// "runs finished in this window".
	"runs_rate": `sum by (result) (increase(ace_run_finish_total{started_by=%s}[$__interval]))`,

	// Per-call pass/fail counts summed across finished runs. Same shape
	// as runs_rate but at call granularity — useful when a run has
	// many calls (e.g. a bursty accept scenario) and the run-level
	// pass/fail is dominated by outliers.
	"calls_rate": `sum by (result) (increase(ace_run_calls_total{started_by=%s}[$__interval]))`,

	// P95 INVITE→200 as a distribution across runs. histogram_quantile
	// over the observed p95 samples gives a "how bad is the worst run"
	// view; the operator wants to spot regressions.
	"invite_p95": `histogram_quantile(0.95, sum by (le) (rate(ace_run_invite_200_p95_ms_bucket{started_by=%s}[$__interval])))`,

	// Last-observed MOS gauge — instantaneous, so use max_over_time
	// to smooth across the step. A drop from ~4.5 to ~3.0 is the
	// clearest audio-quality regression signal.
	"mos_rx": `max_over_time(ace_run_mos_rx_last{started_by=%s}[$__interval])`,

	// Last-observed RTT gauge. Same treatment as MOS.
	"rtt_avg": `max_over_time(ace_run_rtt_avg_ms_last{started_by=%s}[$__interval])`,
}

// promMatrixResponse mirrors just the fields we need from prometheus's
// query_range response so the frontend can render without knowing the
// full response schema.
type promMatrixResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][]interface{}   `json:"values"`
		} `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType,omitempty"`
	Error     string `json:"error,omitempty"`
}

// handleMetricsRange proxies a query_range call to Prometheus using an
// allowlisted panel template + a caller-supplied started_by filter. All
// input is validated: unknown panels return 400, malformed filters are
// PromQL-escaped, and the range is clamped to 90d (matches Prometheus
// retention we ship with).
func (s *Server) handleMetricsRange(c *gin.Context) {
	if s.Cfg.PrometheusURL == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "prometheus not configured (set -prometheus-url)"})
		return
	}

	panel := c.Query("panel")
	tmpl, ok := promPanels[panel]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown panel"})
		return
	}

	startedBy := c.Query("started_by")
	if startedBy == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "started_by required"})
		return
	}
	// The filter goes directly into the PromQL query; escape the two
	// characters that could break out of the label matcher literal
	// (backslash and double-quote). No wildcard support — a bot's
	// started_by is a fixed "bot:<name>" string.
	filter := `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(startedBy) + `"`
	query := strings.ReplaceAll(tmpl, "%s", filter)

	// Range in minutes: default 24h, cap 90d (Prometheus retention).
	rangeMin := 24 * 60
	if v := c.Query("range_min"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90*24*60 {
			rangeMin = n
		}
	}

	// Adaptive step targeting ~500 points per series; floored at the
	// scrape interval (15s) and ceilinged at 1h so pathological ranges
	// don't return millions of samples.
	stepSec := rangeMin * 60 / 500
	if stepSec < 15 {
		stepSec = 15
	}
	if stepSec > 3600 {
		stepSec = 3600
	}
	// Substitute $__interval with the concrete step so the caller's
	// aggregation window matches the sample cadence — otherwise
	// increase() over 15s at a 1h step returns near-zero.
	query = strings.ReplaceAll(query, "$__interval", fmt.Sprintf("%ds", stepSec*4))

	end := time.Now()
	start := end.Add(-time.Duration(rangeMin) * time.Minute)

	q := url.Values{}
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(end.Unix(), 10))
	q.Set("step", strconv.Itoa(stepSec)+"s")

	reqURL := fmt.Sprintf("%s/api/v1/query_range?%s", strings.TrimRight(s.Cfg.PrometheusURL, "/"), q.Encode())
	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Get(reqURL)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("prometheus fetch: %v", err)})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("prometheus read: %v", err)})
		return
	}
	var pm promMatrixResponse
	if err := json.Unmarshal(body, &pm); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("prometheus parse: %v", err)})
		return
	}
	if pm.Status != "success" {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("prometheus %s: %s", pm.ErrorType, pm.Error)})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"panel":  panel,
		"step":   stepSec,
		"result": pm.Data.Result,
	})
}
