package controller

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jchavanton/ace/config"
	"github.com/jchavanton/ace/models"
)

// Cleanup runs the periodic sweep of aged-out run directories and
// oversized alerts.json. One instance per server; ticks once per hour.
// The Runner is consulted (via IsRunActive) so an in-flight run's
// directory is never removed even if its start time is past the cutoff.
type Cleanup struct {
	Cfg    *config.Config
	Runner *Runner

	mu   sync.Mutex
	done chan struct{}
}

// CleanupInterval is how often the sweep fires. Kept coarse — the
// per-run overhead is dominated by directory walks and unlink calls,
// and we don't need minute-level precision for retention.
const CleanupInterval = time.Hour

// Start spawns the sweep goroutine. Safe to call multiple times —
// subsequent calls are no-ops.
func (c *Cleanup) Start() {
	c.mu.Lock()
	if c.done != nil {
		c.mu.Unlock()
		return
	}
	c.done = make(chan struct{})
	done := c.done
	c.mu.Unlock()

	go func() {
		t := time.NewTicker(CleanupInterval)
		defer t.Stop()
		// Fire once on boot so a restart doesn't skip a scheduled sweep.
		c.Run()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				c.Run()
			}
		}
	}()
}

// Stop halts the sweep goroutine. Idempotent.
func (c *Cleanup) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done == nil {
		return
	}
	close(c.done)
	c.done = nil
}

// Run executes one sweep synchronously. Exposed so the /alerts page's
// "Run cleanup now" button can trigger it without waiting for the tick.
// Records the outcome on alert_config.json (LastCleanupAt + summary)
// so the UI can show when the last sweep ran.
func (c *Cleanup) Run() Summary {
	cfg, err := models.LoadAlertConfig(c.Cfg.BotsDir)
	if err != nil {
		log.Printf("ace: cleanup: load alert config: %v", err)
	}

	// Resolve effective retention: config-file value wins over the
	// flag default, -1 means explicitly disabled.
	runsDays := c.Cfg.RunsRetentionDays
	switch cfg.RunsRetentionDays {
	case 0:
		// unset in file → keep flag default
	case -1:
		runsDays = 0 // disabled
	default:
		runsDays = cfg.RunsRetentionDays
	}
	alertsKeep := c.Cfg.AlertsRetentionCount
	switch cfg.AlertsRetentionCount {
	case 0:
	case -1:
		alertsKeep = 0
	default:
		alertsKeep = cfg.AlertsRetentionCount
	}

	var s Summary
	if runsDays > 0 {
		s.RunsRemoved, s.RunsRemovedNames, err = c.sweepRuns(runsDays)
		if err != nil {
			log.Printf("ace: cleanup: sweep runs: %v", err)
			s.Errors = append(s.Errors, err.Error())
		}
	}
	if alertsKeep > 0 {
		s.AlertsRemoved, err = models.TrimAlerts(c.Cfg.BotsDir, alertsKeep)
		if err != nil {
			log.Printf("ace: cleanup: trim alerts: %v", err)
			s.Errors = append(s.Errors, err.Error())
		}
	}

	// Persist the "last sweep" marker on the config so the UI can show
	// it. Errors here are logged but don't fail the sweep — the data on
	// disk is already correctly trimmed.
	cfg.LastCleanupAt = time.Now().UTC()
	cfg.LastCleanupSummary = s.String()
	if err := models.SaveAlertConfig(c.Cfg.BotsDir, cfg); err != nil {
		log.Printf("ace: cleanup: save alert config: %v", err)
	}
	return s
}

// Summary describes one sweep's result. Names are included for the
// UI/log so operators can spot-check what was removed.
type Summary struct {
	RunsRemoved      int
	RunsRemovedNames []string
	AlertsRemoved    int
	Errors           []string
}

// String renders a one-line summary safe to store in alert_config.json
// and show in the /alerts UI.
func (s Summary) String() string {
	parts := []string{
		fmt.Sprintf("runs=%d", s.RunsRemoved),
		fmt.Sprintf("alerts=%d", s.AlertsRemoved),
	}
	if len(s.Errors) > 0 {
		parts = append(parts, fmt.Sprintf("errors=%d", len(s.Errors)))
	}
	return strings.Join(parts, " ")
}

// sweepRuns removes every finished run dir under RunsDir whose
// StartedAt is older than `days`. In-flight runs (status=="running")
// are always kept even if their StartedAt is ancient (defensive against
// a stuck run — we'd rather leak disk than kill a running scenario).
//
// Returns the count and names of removed runs.
func (c *Cleanup) sweepRuns(days int) (int, []string, error) {
	entries, err := os.ReadDir(c.Cfg.RunsDir)
	if err != nil {
		return 0, nil, err
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	var removed []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		run, err := models.LoadRun(c.Cfg.RunsDir, e.Name())
		if err != nil {
			// Corrupt run.json — leave it for the operator to inspect.
			continue
		}
		if run.Status == "running" {
			continue
		}
		if !run.StartedAt.Before(cutoff) {
			continue
		}
		// Defensive: also check the active map. RecoverOrphanedRuns
		// clears stale "running" statuses at boot, but if this
		// controller is re-running that same run somehow, don't yank
		// it. Cheap check; no lock contention since sweeps are hourly.
		if c.Runner != nil && c.Runner.IsRunActive(run.ID) {
			continue
		}
		dir := filepath.Join(c.Cfg.RunsDir, e.Name())
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("ace: cleanup: remove %s: %v", dir, err)
			continue
		}
		removed = append(removed, e.Name())
	}
	return len(removed), removed, nil
}
