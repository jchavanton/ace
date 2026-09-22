package controller

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jchavanton/ace/config"
	"github.com/jchavanton/ace/models"
)

// Scheduler runs enabled bots on their configured interval and files
// alerts when the resulting run fails. One instance per server; owned
// by main and stopped via Stop() on shutdown (currently not called —
// process exit tears everything down).
//
// The tick cadence (TickInterval) is intentionally coarser than the
// finest allowed interval (1 minute): the scheduler compares against
// NextRunAt on every tick and fires anything whose deadline has passed.
// So a 1-minute bot may fire up to TickInterval late — acceptable for
// this app's cadence, and keeps the loop cheap.
type Scheduler struct {
	Cfg    *config.Config
	Runner *Runner

	mu   sync.Mutex
	done chan struct{}
}

// TickInterval is how often the scheduler wakes up to look for due
// bots. Kept short so a bot's actual first-fire latency after creation
// is bounded by this, not by the interval.
const TickInterval = 30 * time.Second

// Start spawns the scheduler goroutine. Safe to call multiple times —
// subsequent calls are no-ops. The goroutine exits when Stop is called.
func (s *Scheduler) Start() {
	s.mu.Lock()
	if s.done != nil {
		s.mu.Unlock()
		return
	}
	s.done = make(chan struct{})
	done := s.done
	s.mu.Unlock()

	// Register the finish hook that turns run outcomes into alerts. If
	// another consumer of OnFinish is added later, chain them here.
	s.Runner.OnFinish = s.onRunFinish

	go func() {
		t := time.NewTicker(TickInterval)
		defer t.Stop()
		// Fire once immediately so a just-created bot with an already-past
		// NextRunAt (e.g. legacy record) is picked up without waiting a full tick.
		s.tick()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s.tick()
			}
		}
	}()
}

// Stop halts the scheduler goroutine. Idempotent.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		return
	}
	close(s.done)
	s.done = nil
}

// tick scans all bots and fires any whose NextRunAt has passed. Bots
// with an in-flight run for the same scenario are skipped this cycle —
// the next tick will retry once the port range is free.
func (s *Scheduler) tick() {
	bots, err := models.ListBots(s.Cfg.BotsDir)
	if err != nil {
		log.Printf("ace: scheduler list bots: %v", err)
		return
	}
	now := time.Now().UTC()
	for i := range bots {
		b := &bots[i]
		if !b.Enabled {
			continue
		}
		if b.IntervalMinutes < 1 {
			continue
		}
		// Zero NextRunAt = never scheduled; treat as "due now" so a
		// newly-enabled bot fires on the next tick without needing a
		// manual save to seed the field.
		if !b.NextRunAt.IsZero() && b.NextRunAt.After(now) {
			continue
		}
		if s.Runner.IsScenarioRunning(b.Scenario) {
			continue
		}
		s.fire(b, now)
	}
}

// fire kicks off a run for the given bot and persists the updated
// bot state (LastRunID, NextRunAt). Errors from Start are logged but
// don't stop the schedule — the next tick will retry.
func (s *Scheduler) fire(b *models.Bot, now time.Time) {
	scn, err := models.LoadScenario(s.Cfg.ScenariosDir, b.Scenario)
	if err != nil {
		// Scenario was deleted under us — log, mark the bot's status,
		// and skip. Operator can disable/delete the bot from the UI.
		log.Printf("ace: bot %q: load scenario %q: %v", b.Name, b.Scenario, err)
		b.LastStatus = "error"
		b.LastRunAt = now
		b.NextRunAt = now.Add(time.Duration(b.IntervalMinutes) * time.Minute)
		_ = models.SaveBot(s.Cfg.BotsDir, b)
		return
	}
	// Bots use the scenario's saved ports (or the runner defaults). No
	// per-bot override in v1 — matches "simple schedule" spec.
	ports := Ports{
		SIP:            scn.Ports.SIP,
		RTPPortStart:   scn.Ports.RTPPortStart,
		RTPPortEnd:     scn.Ports.RTPPortEnd,
		PublicAddress:  scn.Ports.PublicAddress,
		TimeoutSeconds: scn.Ports.TimeoutSeconds,
		Transport:      scn.Ports.Transport,
	}
	trigger := "bot:" + b.Name
	run, err := s.Runner.StartWithTrigger(scn, "scheduler", trigger, ports)
	if err != nil {
		// Port collision or similar — leave NextRunAt in the past a bit
		// so the next tick retries quickly, but not immediately (avoids
		// tight-looping on a persistent conflict).
		log.Printf("ace: bot %q: start: %v", b.Name, err)
		b.LastStatus = "error"
		b.LastRunAt = now
		b.NextRunAt = now.Add(time.Duration(b.IntervalMinutes) * time.Minute)
		_ = models.SaveBot(s.Cfg.BotsDir, b)
		return
	}
	b.LastRunID = run.ID
	b.LastStatus = "running"
	b.LastRunAt = now
	// Schedule the next fire from "now" rather than from LastRunAt+interval.
	// A long-running scenario shouldn't cause back-to-back catch-up fires
	// on completion — the user asked for "every N minutes", not "N
	// minutes of gap between end and start", but starting from now
	// (not run start) is the more conservative reading and matches how
	// most cron implementations behave when the previous job overran.
	b.NextRunAt = now.Add(time.Duration(b.IntervalMinutes) * time.Minute)
	if err := models.SaveBot(s.Cfg.BotsDir, b); err != nil {
		log.Printf("ace: bot %q: save: %v", b.Name, err)
	}
}

// onRunFinish is registered as Runner.OnFinish. It updates the bot's
// LastStatus and files an alert if the run failed.
func (s *Scheduler) onRunFinish(run *models.Run) {
	if run == nil || run.TriggeredBy == "" {
		return
	}
	// TriggeredBy is "bot:<name>" — anything else means a different
	// scheduler tagged the run and this one shouldn't react.
	const prefix = "bot:"
	if len(run.TriggeredBy) <= len(prefix) || run.TriggeredBy[:len(prefix)] != prefix {
		return
	}
	botName := run.TriggeredBy[len(prefix):]
	b, err := models.LoadBot(s.Cfg.BotsDir, botName)
	if err != nil {
		// Bot may have been deleted after firing — nothing to update, but
		// still record the alert against the (now-gone) bot name so the
		// operator can see why it fired.
		b = nil
	}

	failed := run.Status != "done"
	reason := ""
	switch {
	case run.Status == "error":
		reason = "run errored: " + run.Error
	case run.Status == "stopped":
		reason = "run stopped: " + run.Error
	case run.Aggregate.Fail > 0:
		failed = true
		reason = fmt.Sprintf("FAIL %d/%d", run.Aggregate.Fail, run.Aggregate.Total)
	case run.Aggregate.Total == 0:
		// A "done" run with no parsed calls is suspicious — voip_patrol
		// exited cleanly but produced no results. Treat as failure so
		// operators notice missing coverage rather than a green tick.
		failed = true
		reason = "no calls in results"
	}

	if b != nil {
		if failed {
			b.LastStatus = "fail"
		} else {
			b.LastStatus = run.Status
		}
		_ = models.SaveBot(s.Cfg.BotsDir, b)
	}

	// State-machine transition. States are per-bot and persisted in
	// alert_state.json; the scheduler goroutine is the only writer.
	//
	// Semantics:
	//   ok    + failed → firing  (append alert, send email if configured)
	//   firing + failed → firing (reminder email only if past interval)
	//   firing + ok     → ok     (append recovery, send recovery email)
	//   ok    + ok      → ok     (no-op)
	s.transition(botName, run, failed, reason)
}

// transition runs the state machine for one bot after a run completes.
// Kept separate from onRunFinish so the delivery side doesn't crowd
// the tag-and-reason logic.
func (s *Scheduler) transition(botName string, run *models.Run, failed bool, reason string) {
	states, err := models.LoadAlertStates(s.Cfg.BotsDir)
	if err != nil {
		log.Printf("ace: alert state load: %v", err)
		states = map[string]models.AlertState{}
	}
	cfg, err := models.LoadAlertConfig(s.Cfg.BotsDir)
	if err != nil {
		log.Printf("ace: alert config load: %v", err)
	}
	now := time.Now().UTC()
	prev := states[botName]
	next := prev

	switch {
	case failed && prev.State != "firing":
		next.State = "firing"
		next.LastAt = now
		next.LastRunID = run.ID
		next.LastReason = reason
		next.ConsecutiveFails = prev.ConsecutiveFails + 1
		next.LastNotifiedAt = now
		s.appendAlert(botName, run, now, reason)
		s.sendEmail(cfg, botName, run, reason, "firing")

	case failed && prev.State == "firing":
		next.LastAt = now
		next.LastRunID = run.ID
		next.LastReason = reason
		next.ConsecutiveFails = prev.ConsecutiveFails + 1
		// Reminder: only if the operator opted in with a positive
		// interval, and enough time has passed since the last notify.
		if cfg.ReminderIntervalHours > 0 {
			gap := now.Sub(prev.LastNotifiedAt)
			if gap >= time.Duration(cfg.ReminderIntervalHours)*time.Hour {
				next.LastNotifiedAt = now
				s.appendAlert(botName, run, now, "reminder: "+reason)
				s.sendEmail(cfg, botName, run, reason, "reminder")
			}
		}

	case !failed && prev.State == "firing":
		next.State = "ok"
		next.LastAt = now
		next.LastRunID = run.ID
		next.LastReason = ""
		next.ConsecutiveFails = 0
		next.LastNotifiedAt = now
		s.appendAlert(botName, run, now, "recovered")
		s.sendEmail(cfg, botName, run, "recovered", "recovered")

	default:
		// ok + ok — nothing to persist.
		return
	}

	states[botName] = next
	if err := models.SaveAlertStates(s.Cfg.BotsDir, states); err != nil {
		log.Printf("ace: alert state save: %v", err)
	}
}

// appendAlert files one history row. Errors are logged; alert history
// is best-effort — the state machine still records what happened.
func (s *Scheduler) appendAlert(botName string, run *models.Run, at time.Time, reason string) {
	a := models.Alert{
		ID:       models.NewAlertID(at, botName),
		Bot:      botName,
		Scenario: run.Scenario,
		RunID:    run.ID,
		At:       at,
		Reason:   reason,
	}
	if err := models.AppendAlert(s.Cfg.BotsDir, a); err != nil {
		log.Printf("ace: bot %q: append alert: %v", botName, err)
	}
}

// sendEmail wraps SendAlertEmail with subject/body construction and
// error logging. transitionKind is one of "firing", "reminder",
// "recovered" and drives the subject prefix.
func (s *Scheduler) sendEmail(cfg models.AlertConfig, botName string, run *models.Run, reason, transitionKind string) {
	if cfg.SMTPHost == "" {
		return
	}
	var prefix string
	switch transitionKind {
	case "firing":
		prefix = "[ace ALERT]"
	case "reminder":
		prefix = "[ace REMINDER]"
	case "recovered":
		prefix = "[ace RECOVERED]"
	default:
		prefix = "[ace]"
	}
	subject := fmt.Sprintf("%s %s (%s)", prefix, botName, run.Scenario)
	body := buildAlertBody(s.Cfg.PublicBaseURL, botName, run, reason)
	if err := SendAlertEmail(cfg, subject, body); err != nil {
		log.Printf("ace: bot %q: send email: %v", botName, err)
		// Record the delivery failure in history so the operator can see
		// why they didn't get a page. Uses the same alerts.json bucket.
		s.appendAlert(botName, run, time.Now().UTC(), "email failed: "+err.Error())
	}
}
