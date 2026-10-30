// Copyright (C) 2026 Julien Chavanton <jchavanton@gmail.com>
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"log"
	"sync"
	"time"

	"github.com/jchavanton/ace/config"
	"github.com/jchavanton/ace/models"
)

// AutoRunner keeps scenarios flagged `auto_run: true` in the sidecar
// running at all times. Intended for UAS-style scenarios (voip_patrol
// in accept mode with wait ms="-1") that answer calls indefinitely.
//
// Semantics:
//   - On Start, launch any auto-run scenario that isn't already active.
//   - On a run finish (OnFinish hook), re-launch if the finished run
//     was ours (started_by="auto") AND status != "stopped". A
//     user-initiated Stop is sticky: it says "I know what I'm doing,
//     leave it off" — the operator uncheckes AutoRun or restarts ace
//     to re-enable.
//   - Launch retries use a short backoff. voip_patrol crashing in a
//     tight loop (e.g. port conflict with a stale socket, bad XML)
//     shouldn't spin the CPU; the backoff gives transient issues time
//     to clear and makes a persistent problem visible in run history.
type AutoRunner struct {
	Cfg    *config.Config
	Runner *Runner

	mu          sync.Mutex
	done        chan struct{}
	nextRetryAt map[string]time.Time // scenario → when it's eligible to re-launch
}

// autoRunStartedBy is the fixed startedBy label stored on auto-run
// runs. Used by the OnFinish hook to recognize its own runs and by
// the UI / metrics to distinguish auto-starts from scheduler / manual.
const autoRunStartedBy = "auto"

// autoRunBackoff is the delay between a finish and the next re-launch
// attempt. Long enough that a crash-loop stays well under 1 run/sec
// so the run list stays readable; short enough that a brief voip_patrol
// hiccup isn't operator-visible.
const autoRunBackoff = 10 * time.Second

// Start spawns the supervisor. Idempotent. Registers an OnFinish hook
// on the runner and launches any already-flagged scenarios on boot.
func (a *AutoRunner) Start() {
	a.mu.Lock()
	if a.done != nil {
		a.mu.Unlock()
		return
	}
	a.done = make(chan struct{})
	a.nextRetryAt = map[string]time.Time{}
	done := a.done
	a.mu.Unlock()

	a.Runner.AddOnFinish(a.onFinish)

	// Boot pass: launch anything that's flagged and not already running.
	// A separate ticker handles backoff-pending re-launches so that a
	// crash-looping scenario doesn't drag out the boot path.
	go func() {
		// Initial pass fires immediately — scenarios with AutoRun=true
		// start on process launch, not after the first ticker interval.
		a.sweep()
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				a.sweep()
			}
		}
	}()
}

// Stop halts the supervisor goroutine. Idempotent. Does NOT stop the
// auto-run scenarios themselves — that's the operator's call via the
// Stop button on /runs/<id>.
func (a *AutoRunner) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done == nil {
		return
	}
	close(a.done)
	a.done = nil
}

// sweep scans the scenario dir and launches each auto-run scenario
// that isn't already active and whose backoff timer (if any) has
// elapsed. Called on a 5s ticker + once immediately on Start.
func (a *AutoRunner) sweep() {
	scenarios, err := models.LoadScenarios(a.Cfg.ScenariosDir)
	if err != nil {
		log.Printf("ace: auto-runner: load scenarios: %v", err)
		return
	}
	now := time.Now()
	for i := range scenarios {
		scn := &scenarios[i]
		if !scn.Ports.AutoRun {
			continue
		}
		if a.Runner.IsScenarioRunning(scn.Name) {
			// Already up (either booted earlier this process or started
			// manually and still running). Nothing to do.
			continue
		}
		a.mu.Lock()
		next, hasRetry := a.nextRetryAt[scn.Name]
		a.mu.Unlock()
		if hasRetry && now.Before(next) {
			continue
		}
		a.launch(scn)
	}
}

// launch starts one auto-run attempt. Any error (port conflict, bad
// XML) is logged and the backoff timer is set — sweep will retry.
func (a *AutoRunner) launch(scn *models.Scenario) {
	ports := Ports{
		SIP:            scn.Ports.SIP,
		RTPPortStart:   scn.Ports.RTPPortStart,
		RTPPortEnd:     scn.Ports.RTPPortEnd,
		PublicAddress:  scn.Ports.PublicAddress,
		TimeoutSeconds: scn.Ports.TimeoutSeconds,
		Transport:      scn.Ports.Transport,
		Nameservers:    scn.Ports.Nameservers,
	}
	// UAS scenarios want "unlimited" even if the operator didn't set it
	// explicitly — the whole point of auto-run is "stay up". -1 is the
	// runner's convention for no deadline. Honor an explicitly positive
	// per-scenario timeout though: an operator who typed 3600s there
	// knows what they're doing.
	if ports.TimeoutSeconds == 0 {
		ports.TimeoutSeconds = -1
	}
	run, err := a.Runner.StartWithTrigger(scn, autoRunStartedBy, autoRunStartedBy, ports)
	if err != nil {
		log.Printf("ace: auto-runner: start %q: %v (backing off %s)", scn.Name, err, autoRunBackoff)
		a.mu.Lock()
		a.nextRetryAt[scn.Name] = time.Now().Add(autoRunBackoff)
		a.mu.Unlock()
		return
	}
	log.Printf("ace: auto-runner: started %q as %s", scn.Name, run.ID)
	// Clear any previous backoff — a successful launch resets the
	// crash-loop counter.
	a.mu.Lock()
	delete(a.nextRetryAt, scn.Name)
	a.mu.Unlock()
}

// onFinish is registered as a Runner.AddOnFinish callback. When one
// of our auto-runs finishes, schedule a re-launch — unless the operator
// explicitly stopped it (sticky Stop) or AutoRun has been turned off
// since we started it.
func (a *AutoRunner) onFinish(run *models.Run) {
	if run.StartedBy != autoRunStartedBy {
		return
	}
	if run.Status == "stopped" {
		log.Printf("ace: auto-runner: %q stopped by operator; not restarting", run.Scenario)
		return
	}
	// Re-check the sidecar — the operator may have toggled AutoRun off
	// while the run was in flight.
	scn, err := models.LoadScenario(a.Cfg.ScenariosDir, run.Scenario)
	if err != nil || !scn.Ports.AutoRun {
		return
	}
	a.mu.Lock()
	a.nextRetryAt[run.Scenario] = time.Now().Add(autoRunBackoff)
	a.mu.Unlock()
	// Don't re-launch inline from the finish goroutine — sweep will
	// pick it up on the next tick. Keeps the OnFinish callback cheap
	// and lets backoff pacing work uniformly.
}
