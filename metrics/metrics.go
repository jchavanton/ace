// Copyright (C) 2026 Julien Chavanton <jchavanton@gmail.com>
// SPDX-License-Identifier: Apache-2.0

// Package metrics registers ace's prometheus collectors and exposes a
// small Observe helper the runner calls when a run finishes.
//
// Cardinality budget: scenario × started_by × result. Scenarios and bots
// are hand-created; `started_by` is either "bot:<name>" or an operator
// email/anonymous. That keeps the label space bounded to what an operator
// deploys — an accidental unique-per-user email won't blow the series
// count on the dashboards we render, and the /bots/:name/report page
// filters by started_by so the panels stay per-bot even if the operator
// added other users.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/jchavanton/ace/models"
)

var (
	runFinishTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ace_run_finish_total",
		Help: "Runs that reached a terminal state, labelled by outcome.",
	}, []string{"scenario", "started_by", "result"})

	runCallsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ace_run_calls_total",
		Help: "Per-call outcomes summed across finished runs.",
	}, []string{"scenario", "started_by", "result"})

	// P95 already p95'd inside a run; observing that value in a histogram
	// gives a distribution across runs, which is what the dashboard
	// actually wants (variance from run to run, not per-call).
	runInvite200P95 = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ace_run_invite_200_p95_ms",
		Help:    "Distribution of per-run p95 INVITE→200 latency (ms).",
		Buckets: []float64{50, 100, 200, 400, 800, 1600, 3200, 6400, 12800},
	}, []string{"scenario", "started_by"})

	// Gauges hold the last observation. Enough for at-a-glance summaries
	// on the report page; historical trends come from
	// last_over_time(<gauge>[$__interval]) rather than a separate series.
	runMOSRxLast = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ace_run_mos_rx_last",
		Help: "Average Rx MOS reported by the most recent finished run.",
	}, []string{"scenario", "started_by"})

	runRTTLast = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ace_run_rtt_avg_ms_last",
		Help: "Average RTT (ms) reported by the most recent finished run.",
	}, []string{"scenario", "started_by"})
)

// runResult collapses (Status, aggregate.Fail>0) into a single label so
// the PromQL for the dashboards is straightforward. "mixed" only appears
// when at least one call passed and at least one failed — a healthy bot
// stays on "pass" and a broken one goes to "fail" or "error".
func runResult(r *models.Run) string {
	switch r.Status {
	case "error":
		return "error"
	case "stopped":
		return "stopped"
	}
	if r.Aggregate.Total == 0 {
		return "empty"
	}
	if r.Aggregate.Fail == 0 {
		return "pass"
	}
	if r.Aggregate.Pass == 0 {
		return "fail"
	}
	return "mixed"
}

// startedBy returns the label to use for the "started_by" dimension.
// Bot runs stamp `TriggeredBy: "bot:<name>"`; interactive runs stamp
// `StartedBy` with the operator's email. Both flow into the same label
// so per-bot dashboards can filter on `started_by="bot:X"` and there's
// no separate metric per source.
func startedBy(r *models.Run) string {
	if r.TriggeredBy != "" {
		return r.TriggeredBy
	}
	if r.StartedBy != "" {
		return r.StartedBy
	}
	return "anonymous"
}

// Observe records a single finished run's outcome. Safe to call once per
// run at the finalize block in Runner.Start.
func Observe(r *models.Run) {
	if r == nil {
		return
	}
	scenario := r.Scenario
	sb := startedBy(r)
	result := runResult(r)

	runFinishTotal.WithLabelValues(scenario, sb, result).Inc()

	// Per-call counters take the run's outcome as the "result" label so
	// pass+fail sum to Aggregate.Total even on partial parses.
	if r.Aggregate.Pass > 0 {
		runCallsTotal.WithLabelValues(scenario, sb, "pass").Add(float64(r.Aggregate.Pass))
	}
	if r.Aggregate.Fail > 0 {
		runCallsTotal.WithLabelValues(scenario, sb, "fail").Add(float64(r.Aggregate.Fail))
	}

	// Only record latency + MOS when we actually parsed calls; a run that
	// errored before voip_patrol produced results.json has zeroed
	// aggregates that would poison the p95 histogram with a phantom 0ms
	// sample.
	if r.Aggregate.Total > 0 {
		runInvite200P95.WithLabelValues(scenario, sb).Observe(float64(r.Aggregate.Invite200P95))
		runMOSRxLast.WithLabelValues(scenario, sb).Set(r.Aggregate.MOSAvgRx)
		runRTTLast.WithLabelValues(scenario, sb).Set(float64(r.Aggregate.RTTAvgMs))
	}
}
