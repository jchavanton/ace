// Copyright (C) 2026 Julien Chavanton <jchavanton@gmail.com>
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jchavanton/ace/models"
)

// ReaggregateResult reports what one run's backfill would (or did) do.
type ReaggregateResult struct {
	ID      string
	Before  models.Aggregate
	After   models.Aggregate
	Changed bool
	Skipped string // non-empty = skipped, reason here
	Error   error
}

// ReaggregateRuns recomputes Aggregate for every run on disk using the
// live aggregate() function and compares it to what's stored. When
// apply=false, it only reports; when apply=true, it writes each changed
// run.json after copying the old one to run.json.bak-<yyyymmdd>.
//
// Skips runs that are still "running" or lack a Calls slice (nothing to
// re-aggregate from). Caller prints the returned slice.
func (r *Runner) ReaggregateRuns(apply bool) ([]ReaggregateResult, error) {
	runs, err := models.ListRuns(r.Cfg.RunsDir)
	if err != nil {
		return nil, err
	}
	stamp := time.Now().UTC().Format("20060102")
	out := make([]ReaggregateResult, 0, len(runs))
	for _, run := range runs {
		res := ReaggregateResult{ID: run.ID, Before: run.Aggregate}
		switch {
		case run.Status == "running":
			res.Skipped = "status=running"
			out = append(out, res)
			continue
		case len(run.Calls) == 0:
			res.Skipped = "no calls to aggregate"
			out = append(out, res)
			continue
		}
		res.After = aggregate(run.Calls)
		res.Changed = res.Before != res.After
		if !res.Changed || !apply {
			out = append(out, res)
			continue
		}
		dir := run.Dir(r.Cfg.RunsDir)
		src := filepath.Join(dir, "run.json")
		bak := filepath.Join(dir, "run.json.bak-"+stamp)
		if err := copyFile(src, bak); err != nil {
			res.Error = fmt.Errorf("backup: %w", err)
			out = append(out, res)
			continue
		}
		run.Aggregate = res.After
		if err := run.Save(r.Cfg.RunsDir); err != nil {
			res.Error = fmt.Errorf("save: %w", err)
		}
		out = append(out, res)
	}
	return out, nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	// Preserve the JSON mode (0644 matches what Run.Save writes).
	return os.WriteFile(dst, b, 0o644)
}

// AggregateFieldsString renders the tone+pass/fail deltas that matter
// for this backfill, in a format a shell can read at a glance.
func (res ReaggregateResult) Diff() string {
	b, a := res.Before, res.After
	parts := []string{}
	if b.ToneDetectedMs != a.ToneDetectedMs {
		parts = append(parts, fmt.Sprintf("tone_ms %d→%d", b.ToneDetectedMs, a.ToneDetectedMs))
	}
	if b.ToneDetectedCalls != a.ToneDetectedCalls {
		parts = append(parts, fmt.Sprintf("tone_calls %d→%d", b.ToneDetectedCalls, a.ToneDetectedCalls))
	}
	if b.ToneState != a.ToneState {
		parts = append(parts, fmt.Sprintf("tone_state %q→%q", b.ToneState, a.ToneState))
	}
	if len(parts) == 0 {
		return "(no tone field changes)"
	}
	buf, _ := json.Marshal(parts)
	return string(buf)
}
