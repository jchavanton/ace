// Package controller drives voip_patrol: spawns the binary, captures
// its stdout/stderr + results.json, parses the per-call JSON, computes
// aggregates, and persists everything under $RunsDir/<run-id>/.
//
// v1 keeps runs serialized (one at a time) — voip_patrol binds a single
// SIP port, so two concurrent runs would collide. A sync.Mutex enforces
// this; the UI shows "busy" when a run is already in flight.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jchavanton/ace/config"
	"github.com/jchavanton/ace/models"
)

// Runner spawns voip_patrol and processes its output. One per server;
// the Mutex serializes runs.
type Runner struct {
	Cfg *config.Config
	mu  sync.Mutex
}

// IsBusy reports whether a run is currently in flight.
func (r *Runner) IsBusy() bool {
	if r.mu.TryLock() {
		r.mu.Unlock()
		return false
	}
	return true
}

// Run executes the named scenario, blocking until voip_patrol exits.
// Returns the persisted Run record (which holds parsed call results and
// any error). Caller-side timeouts can use ctx; voip_patrol itself has
// no built-in timeout, so a stuck run would otherwise block forever.
func (r *Runner) Run(ctx context.Context, scenario *models.Scenario) (*models.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	run, err := models.NewRun(r.Cfg.RunsDir, scenario.Name)
	if err != nil {
		return nil, fmt.Errorf("new run: %w", err)
	}

	args := []string{
		"--port", fmt.Sprintf("%d", r.Cfg.VoipPatrolPort),
		"-c", scenario.Path,
	}
	if r.Cfg.PublicAddress != "" {
		args = append(args, "--public-address", r.Cfg.PublicAddress)
	}

	// voip_patrol writes results.json + record_*.wav to its cwd. Setting
	// Dir to the run's output dir keeps every artifact bundled per-run
	// without naming-collision juggling.
	cmd := exec.CommandContext(ctx, r.Cfg.VoipPatrolBin, args...)
	cmd.Dir = run.Dir(r.Cfg.RunsDir)

	logPath := filepath.Join(run.Dir(r.Cfg.RunsDir), "stdout.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		run.Status = "error"
		run.Error = fmt.Sprintf("create log: %v", err)
		_ = run.Save(r.Cfg.RunsDir)
		return run, err
	}
	defer logFile.Close()
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	runErr := cmd.Run()
	run.FinishedAt = time.Now().UTC()
	if cmd.ProcessState != nil {
		run.ExitCode = cmd.ProcessState.ExitCode()
	}
	if runErr != nil && run.ExitCode == 0 {
		// Probably ctx cancellation; surface it.
		run.Status = "error"
		run.Error = runErr.Error()
	}

	// Parse results.json regardless of exit code — voip_patrol writes
	// per-call results as they complete, so a crash mid-run still
	// leaves partial data we want to surface.
	calls, parseErr := loadVoipPatrolResults(filepath.Join(run.Dir(r.Cfg.RunsDir), "results.json"), scenario.Name)
	run.Calls = calls
	run.Aggregate = aggregate(calls)
	if run.Status != "error" {
		run.Status = "done"
	}
	if parseErr != nil && len(calls) == 0 {
		// Only surface parse errors when there's no usable data; a
		// partial-result parse is normal and shouldn't read as fail.
		run.Error = fmt.Sprintf("parse results: %v", parseErr)
	}

	if err := run.Save(r.Cfg.RunsDir); err != nil {
		return run, fmt.Errorf("save run: %w", err)
	}
	return run, nil
}

// loadVoipPatrolResults reads voip_patrol's results.json. The file is
// **one JSON object per line** (not a JSON array — see the README), so
// we decode line-by-line. We filter to the calls matching this run's
// scenario label, since voip_patrol appends to the file across runs
// when its cwd doesn't change. Cwd is per-run here so the filter is
// belt-and-suspenders.
func loadVoipPatrolResults(path, label string) ([]models.CallResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []models.CallResult
	dec := json.NewDecoder(f)
	for {
		var c models.CallResult
		err := dec.Decode(&c)
		if err == io.EOF {
			break
		}
		if err != nil {
			// Skip malformed lines rather than failing the parse —
			// the file is append-only and a torn write at the tail
			// is recoverable.
			break
		}
		if label != "" && c.Label != label {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// aggregate computes the run-level summary from per-call results.
// Empty input returns a zero-valued Aggregate.
func aggregate(calls []models.CallResult) models.Aggregate {
	agg := models.Aggregate{Total: len(calls)}
	if len(calls) == 0 {
		return agg
	}
	var (
		invite200 = make([]int, 0, len(calls))
		mosRx     = 0.0
		mosTx     = 0.0
		rttSum    = 0
		rttN      = 0
		mosN      = 0
	)
	for _, c := range calls {
		if strings.EqualFold(c.Result, "PASS") {
			agg.Pass++
		} else {
			agg.Fail++
		}
		if c.SIPLatency.Invite200Ms > 0 {
			invite200 = append(invite200, c.SIPLatency.Invite200Ms)
		}
		for _, rs := range c.RTPStats {
			if rs.Rx.MosLQ > 0 {
				mosRx += rs.Rx.MosLQ
				mosTx += rs.Tx.MosLQ
				mosN++
			}
			if rs.RTT > 0 {
				rttSum += rs.RTT
				rttN++
			}
			agg.PacketsLossTx += rs.Tx.Loss
		}
	}
	agg.Invite200P50 = percentile(invite200, 50)
	agg.Invite200P95 = percentile(invite200, 95)
	if mosN > 0 {
		agg.MOSAvgRx = mosRx / float64(mosN)
		agg.MOSAvgTx = mosTx / float64(mosN)
	}
	if rttN > 0 {
		agg.RTTAvgMs = rttSum / rttN
	}
	return agg
}

// percentile returns the p-th percentile of xs using the nearest-rank
// method. 0 for empty input. Mutates a copy, not the caller's slice.
func percentile(xs []int, p int) int {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int(nil), xs...)
	sort.Ints(s)
	idx := (p * len(s)) / 100
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}
