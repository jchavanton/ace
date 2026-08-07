// Package controller drives voip_patrol: spawns the binary, captures
// its stdout/stderr + results.json, parses the per-call JSON, computes
// aggregates, and persists everything under $RunsDir/<run-id>/.
//
// Runs can execute concurrently as long as their SIP port and RTP
// range don't overlap with any other in-flight run. The Runner keeps
// a small in-memory registry of active runs and refuses to Start a
// new one whose ports would collide.
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
// activeMu guards the registry of in-flight runs.
type Runner struct {
	Cfg *config.Config

	activeMu sync.Mutex
	active   map[string]*activeRun // keyed by Run.ID
}

// activeRun is the in-memory record of a currently-executing run.
// Lifetime: registered in Start (under activeMu, after canAllocate
// succeeds), removed by execute's defer when voip_patrol exits.
type activeRun struct {
	id           string
	scenario     string
	sipPort      int
	rtpPortStart int
	rtpPortEnd   int
	startedAt    time.Time
	startedBy    string
	cancel       context.CancelFunc
	stoppedBy    string // set by Stop, read by execute to record the run's Error
}

// ActiveRun is the exported view of an in-flight run, safe for the UI
// to read. Snapshotted under activeMu so callers don't share state
// with the goroutine.
type ActiveRun struct {
	ID           string
	Scenario     string
	SIPPort      int
	RTPPortStart int
	RTPPortEnd   int
	StartedAt    time.Time
	StartedBy    string
}

// ActiveRuns returns a snapshot of all runs currently in flight,
// sorted by StartedAt ascending. Empty when nothing is running.
func (r *Runner) ActiveRuns() []ActiveRun {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	out := make([]ActiveRun, 0, len(r.active))
	for _, a := range r.active {
		out = append(out, ActiveRun{
			ID:           a.id,
			Scenario:     a.scenario,
			SIPPort:      a.sipPort,
			RTPPortStart: a.rtpPortStart,
			RTPPortEnd:   a.rtpPortEnd,
			StartedAt:    a.startedAt,
			StartedBy:    a.startedBy,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// IsScenarioRunning reports whether at least one active run is
// executing the given scenario. Used by the scenario-delete handler
// to refuse deletion of a scenario that's currently in use, and by
// the scenario-detail template to disable the local Run button when
// this scenario's already running with the default ports.
func (r *Runner) IsScenarioRunning(scenario string) bool {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	for _, a := range r.active {
		if a.scenario == scenario {
			return true
		}
	}
	return false
}

// DefaultPortsInUse reports whether any active run occupies the
// runner's default SIP port or overlaps its default RTP range. The
// scenarios-list "Run" button uses this to decide whether to gray
// itself out — since that button submits without per-run overrides,
// starting it while defaults are taken would just 409.
func (r *Runner) DefaultPortsInUse() bool {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	for _, a := range r.active {
		if a.sipPort == r.Cfg.VoipPatrolPort {
			return true
		}
		if rangesOverlap(a.rtpPortStart, a.rtpPortEnd, r.Cfg.RTPPortStart, r.Cfg.RTPPortEnd) {
			return true
		}
	}
	return false
}

// canAllocate returns nil iff the requested ports don't overlap with
// any currently-active run. Called under activeMu (the caller holds
// it and immediately registers the run on success — the check + insert
// must be atomic or two racing Start()s could both pass).
func (r *Runner) canAllocate(sip, rtpStart, rtpEnd int) error {
	if sip <= 0 || rtpStart <= 0 || rtpEnd <= 0 {
		return fmt.Errorf("invalid ports (sip=%d rtp=%d-%d)", sip, rtpStart, rtpEnd)
	}
	if rtpStart > rtpEnd {
		return fmt.Errorf("rtp start %d > end %d", rtpStart, rtpEnd)
	}
	// Deliberately no check for SIP-inside-RTP-range. In practice SIP
	// binds one specific port (5060, 5093, etc.) and voip_patrol's RTP
	// pool grabs from a wide range that often numerically contains it.
	// pjsip doesn't allocate the SIP port for RTP, so there's no real
	// conflict. Rejecting this combination breaks the common case of
	// SIP 5060 with a big RTP pool.
	for _, a := range r.active {
		if sip == a.sipPort {
			return fmt.Errorf("SIP port %d already in use by run %s", sip, a.id)
		}
		if rangesOverlap(rtpStart, rtpEnd, a.rtpPortStart, a.rtpPortEnd) {
			return fmt.Errorf("RTP range %d-%d overlaps run %s (using %d-%d)",
				rtpStart, rtpEnd, a.id, a.rtpPortStart, a.rtpPortEnd)
		}
	}
	return nil
}

// rangesOverlap returns true when [a1,a2] intersects [b1,b2].
// Callers guarantee a1<=a2 and b1<=b2.
func rangesOverlap(a1, a2, b1, b2 int) bool {
	return a1 <= b2 && b1 <= a2
}

// Stop cancels the specified in-flight run's voip_patrol process.
// Returns an error if id doesn't match a currently-active run (so a
// stale browser tab firing /runs/<old-id>/stop can't kill an
// unrelated new run). stoppedBy is stamped into the run's Error field
// for audit; execute() consumes it when the process exits.
func (r *Runner) Stop(id, stoppedBy string) error {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	a, ok := r.active[id]
	if !ok {
		return fmt.Errorf("run %q is not currently running", id)
	}
	a.stoppedBy = stoppedBy
	// Cancelling the context triggers exec.CommandContext to signal the
	// child (SIGKILL by default on Unix) and Run() returns with a
	// non-nil err. execute() sees stoppedBy and records Status=stopped.
	a.cancel()
	return nil
}

// RecoverOrphanedRuns scans RunsDir at boot and marks every run whose
// run.json still says status=running as "error" with an "orphaned by
// controller restart" note. Called once at startup, before HTTP starts
// serving, so the UI never sees a fake "running" from a prior instance
// (real in-flight runs die with the process — no ACE reattach path).
// Also parses any results.json the dead voip_patrol left behind so the
// PASS/FAIL breakdown for completed calls is preserved, matching what
// execute() does at normal exit.
//
// Errors on individual runs are logged and swallowed — one broken
// run.json shouldn't block ACE startup.
func (r *Runner) RecoverOrphanedRuns() (int, error) {
	entries, err := os.ReadDir(r.Cfg.RunsDir)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	recovered := 0
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		run, err := models.LoadRun(r.Cfg.RunsDir, e.Name())
		if err != nil {
			continue
		}
		if run.Status != "running" {
			continue
		}
		run.Status = "error"
		run.Error = "orphaned by controller restart"
		run.FinishedAt = now
		if calls, _ := loadVoipPatrolResults(filepath.Join(run.Dir(r.Cfg.RunsDir), "results.json")); len(calls) > 0 {
			run.Calls = calls
			run.Aggregate = aggregate(calls)
		}
		if err := run.Save(r.Cfg.RunsDir); err != nil {
			continue
		}
		recovered++
	}
	return recovered, nil
}

// Ports is a per-run override of the SIP + RTP ports voip_patrol binds
// plus the public IP it advertises. Zero fields (empty string for
// PublicAddress) fall back to the runner's config defaults, so a caller
// that doesn't care can pass a zero Ports{}.
type Ports struct {
	SIP           int
	RTPPortStart  int
	RTPPortEnd    int
	PublicAddress string
}

// Start kicks off a scenario run and returns the freshly-created Run
// record (status=running). The actual voip_patrol execution proceeds
// in a background goroutine using a context that's independent of the
// HTTP request — so navigating away from the page or closing the tab
// doesn't kill the run.
//
// startedBy is the authenticated user (from the auth middleware) or
// "" when auth is off. Persisted to run.json for audit; the runner
// itself doesn't use it.
//
// ports overrides the SIP/RTP ports for this run only. Zero fields =
// use the runner's config defaults.
//
// Concurrent runs are allowed as long as their SIP port and RTP range
// don't overlap with any currently-active run. On collision (or any
// pre-exec error), Start returns an error and no Run; the goroutine
// itself never returns an error — failures land in run.Status /
// run.Error inside run.json.
func (r *Runner) Start(scenario *models.Scenario, startedBy string, ports Ports) (*models.Run, error) {
	sip := firstNonZero(ports.SIP, r.Cfg.VoipPatrolPort)
	rtpStart := firstNonZero(ports.RTPPortStart, r.Cfg.RTPPortStart)
	rtpEnd := firstNonZero(ports.RTPPortEnd, r.Cfg.RTPPortEnd)
	publicAddr := ports.PublicAddress
	if publicAddr == "" {
		publicAddr = r.Cfg.PublicAddress
	}

	// Create the context before taking activeMu so we can register its
	// cancel with the activeRun record atomically. Same 10-minute
	// upper bound as before; execute()'s defer calls the same cancel
	// on the way out to release the timer.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)

	// canAllocate + insert must be atomic; two concurrent Starts must
	// not both pass the check and both register.
	r.activeMu.Lock()
	if err := r.canAllocate(sip, rtpStart, rtpEnd); err != nil {
		r.activeMu.Unlock()
		cancel()
		return nil, err
	}

	run, err := models.NewRun(r.Cfg.RunsDir, scenario.Name)
	if err != nil {
		r.activeMu.Unlock()
		cancel()
		return nil, fmt.Errorf("new run: %w", err)
	}
	run.StartedBy = startedBy
	// Record the effective ports (after fallback) on the run itself so
	// the detail page can show what voip_patrol actually bound and old
	// runs remain distinguishable after a config change.
	run.SIPPort = sip
	run.RTPPortStart = rtpStart
	run.RTPPortEnd = rtpEnd
	run.PublicAddress = publicAddr

	if r.active == nil {
		r.active = make(map[string]*activeRun)
	}
	r.active[run.ID] = &activeRun{
		id:           run.ID,
		scenario:     scenario.Name,
		sipPort:      sip,
		rtpPortStart: rtpStart,
		rtpPortEnd:   rtpEnd,
		startedAt:    run.StartedAt,
		startedBy:    startedBy,
		cancel:       cancel,
	}
	r.activeMu.Unlock()

	// Persist the running-state record so the UI can show it
	// immediately after the redirect. Done after registering in the
	// active map so IsScenarioRunning / DefaultPortsInUse see it
	// consistently.
	if err := run.Save(r.Cfg.RunsDir); err != nil {
		r.activeMu.Lock()
		delete(r.active, run.ID)
		r.activeMu.Unlock()
		cancel()
		return nil, fmt.Errorf("save run: %w", err)
	}

	go r.execute(ctx, cancel, run, scenario)
	return run, nil
}

func firstNonZero(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// execute is the blocking half: spawns voip_patrol, waits for exit,
// parses results, saves the final run.json. Runs on a background
// goroutine — the http handler that invoked Start has long since
// returned by the time this finishes.
//
// ctx/cancel are owned by Start (registered in the active map so
// Stop can cancel this run). execute cancels once on the way out to
// release the timeout timer and unregisters the run.
func (r *Runner) execute(ctx context.Context, cancel context.CancelFunc, run *models.Run, scenario *models.Scenario) {
	defer cancel()
	// Unregister from the active map exactly once, when we're done.
	// A goroutine holding stale a.cancel is harmless: cancelling an
	// already-cancelled ctx is a no-op.
	defer func() {
		r.activeMu.Lock()
		delete(r.active, run.ID)
		r.activeMu.Unlock()
	}()

	runDir := run.Dir(r.Cfg.RunsDir)

	// voip_patrol scenarios reference WAVs by relative path
	// ("voice_ref_files/reference_8000.wav"), resolved against cwd.
	// We set cwd = runDir, so symlink the shared voice_ref_files dir
	// into runDir under that exact name. Best-effort: an error here
	// just means scenarios needing ref files will fail with the
	// underlying "not found" — same as before this fix.
	if r.Cfg.VoiceRefDir != "" {
		link := filepath.Join(runDir, "voice_ref_files")
		if err := os.Symlink(r.Cfg.VoiceRefDir, link); err != nil && !os.IsExist(err) {
			// Log to the run's stdout.log via a placeholder — we can't
			// open logFile yet (needed for cmd wiring below). Keep going.
			fmt.Fprintf(os.Stderr, "ace: symlink voice_ref_files: %v\n", err)
		}
	}

	args := []string{
		"--port", fmt.Sprintf("%d", run.SIPPort),
		"--rtp-port", fmt.Sprintf("%d", run.RTPPortStart),
		"--rtp-port-end", fmt.Sprintf("%d", run.RTPPortEnd),
		"-c", scenario.Path,
		"--record-dir", runDir,
	}
	if run.PublicAddress != "" {
		args = append(args, "--ip-addr", run.PublicAddress)
	}

	// voip_patrol writes results.json to cwd; recordings go to --record-dir.
	// We set both to the run dir so every artifact lands bundled per-run.
	cmd := exec.CommandContext(ctx, r.Cfg.VoipPatrolBin, args...)
	cmd.Dir = runDir

	logPath := filepath.Join(run.Dir(r.Cfg.RunsDir), "stdout.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		run.Status = "error"
		run.Error = fmt.Sprintf("create log: %v", err)
		run.FinishedAt = time.Now().UTC()
		_ = run.Save(r.Cfg.RunsDir)
		return
	}
	defer logFile.Close()
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	runErr := cmd.Run()
	run.FinishedAt = time.Now().UTC()
	if cmd.ProcessState != nil {
		run.ExitCode = cmd.ProcessState.ExitCode()
	}
	// Exec-level failures (binary not found, ctx-canceled, signal-killed
	// before producing output) win over downstream parse errors — they're
	// the actual root cause. An explicit Stop() gets its own status so
	// the UI can render it distinctly and the audit trail records who
	// hit the button.
	r.activeMu.Lock()
	stoppedBy := ""
	if a, ok := r.active[run.ID]; ok {
		stoppedBy = a.stoppedBy
	}
	r.activeMu.Unlock()
	if stoppedBy != "" {
		run.Status = "stopped"
		run.Error = fmt.Sprintf("stopped by %s", stoppedBy)
	} else if runErr != nil {
		run.Status = "error"
		run.Error = fmt.Sprintf("voip_patrol: %v", runErr)
	}

	// Parse results.json regardless of exit code — voip_patrol writes
	// per-call results as they complete, so a crash mid-run still
	// leaves partial data we want to surface.
	calls, parseErr := loadVoipPatrolResults(filepath.Join(run.Dir(r.Cfg.RunsDir), "results.json"))
	run.Calls = calls
	run.Aggregate = aggregate(calls)
	if run.Status != "error" && run.Status != "stopped" {
		run.Status = "done"
		if parseErr != nil && len(calls) == 0 {
			// Only surface parse errors when nothing else went wrong AND
			// we have no usable data; partial-result parse is normal.
			run.Status = "error"
			run.Error = fmt.Sprintf("parse results: %v", parseErr)
		}
	}

	_ = run.Save(r.Cfg.RunsDir)
}

// loadVoipPatrolResults reads voip_patrol's results.json. The file is
// **one JSON object per line** (not a JSON array — see the README),
// so we decode line-by-line. Since we set cwd to the per-run dir when
// spawning voip_patrol, each run gets a fresh file holding only its
// own calls — no need to filter by label (and label-filtering bit us
// before: the XML's label= attribute is operator-chosen and doesn't
// have to match the scenario filename).
func loadVoipPatrolResults(path string) ([]models.CallResult, error) {
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
