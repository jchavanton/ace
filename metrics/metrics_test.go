// Copyright (C) 2026 Julien Chavanton <jchavanton@gmail.com>
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/jchavanton/ace/models"
)

func TestObserveEmitsExpectedSeries(t *testing.T) {
	Observe(&models.Run{
		Scenario:    "smoke",
		TriggeredBy: "bot:test",
		Status:      "done",
		Aggregate: models.Aggregate{
			Total: 10, Pass: 8, Fail: 2,
			Invite200P95: 320,
			MOSAvgRx:     4.3,
			RTTAvgMs:     45,
		},
	})

	// Result label should reflect mixed pass+fail. Counter is +1 for the
	// finish and +8/+2 for calls.
	if got := testutil.ToFloat64(runFinishTotal.WithLabelValues("smoke", "bot:test", "mixed")); got != 1 {
		t.Errorf("runFinishTotal mixed = %v, want 1", got)
	}
	if got := testutil.ToFloat64(runCallsTotal.WithLabelValues("smoke", "bot:test", "pass")); got != 8 {
		t.Errorf("runCallsTotal pass = %v, want 8", got)
	}
	if got := testutil.ToFloat64(runCallsTotal.WithLabelValues("smoke", "bot:test", "fail")); got != 2 {
		t.Errorf("runCallsTotal fail = %v, want 2", got)
	}
	if got := testutil.ToFloat64(runMOSRxLast.WithLabelValues("smoke", "bot:test")); got != 4.3 {
		t.Errorf("runMOSRxLast = %v, want 4.3", got)
	}
}

func TestRunResultCoversStatusStates(t *testing.T) {
	cases := []struct {
		name string
		run  *models.Run
		want string
	}{
		{"error status wins", &models.Run{Status: "error", Aggregate: models.Aggregate{Total: 3, Pass: 3}}, "error"},
		{"stopped status wins", &models.Run{Status: "stopped", Aggregate: models.Aggregate{Total: 3, Pass: 3}}, "stopped"},
		{"empty aggregate", &models.Run{Status: "done"}, "empty"},
		{"all pass", &models.Run{Status: "done", Aggregate: models.Aggregate{Total: 5, Pass: 5}}, "pass"},
		{"all fail", &models.Run{Status: "done", Aggregate: models.Aggregate{Total: 5, Fail: 5}}, "fail"},
		{"mixed", &models.Run{Status: "done", Aggregate: models.Aggregate{Total: 5, Pass: 3, Fail: 2}}, "mixed"},
	}
	for _, tc := range cases {
		if got := runResult(tc.run); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestObserveIgnoresZeroAggregate prevents phantom 0ms latency and
// 0.0 MOS samples on runs that errored before results.json existed
// (would poison the p95 histogram + the MOS gauge).
func TestObserveIgnoresZeroAggregate(t *testing.T) {
	Observe(&models.Run{
		Scenario: "empty",
		Status:   "error",
		// Total=0, so no latency/MOS should be observed.
	})
	// Collect the histogram directly and scan its label sets for
	// scenario="empty". Other tests in this package populate the
	// histogram with scenario="smoke", so we assert the negative
	// (no "empty" sample) rather than an absolute count.
	ch := make(chan prometheus.Metric, 32)
	runInvite200P95.Collect(ch)
	close(ch)
	for m := range ch {
		var pm dto.Metric
		if err := m.Write(&pm); err != nil {
			t.Fatalf("metric write: %v", err)
		}
		for _, l := range pm.Label {
			if l.GetName() == "scenario" && l.GetValue() == "empty" {
				t.Errorf("empty run leaked into latency histogram")
			}
		}
	}
}
