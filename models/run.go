package models

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Run is one execution of a scenario. Persisted as a directory under
// $RunsDir/<timestamp>-<scenario>/, containing:
//
//	run.json        controller-side metadata (this struct, serialized)
//	results.json    voip_patrol's raw per-call output
//	stdout.log      voip_patrol's stdout/stderr (truncated to last N lines)
//	*.wav           per-leg recordings, if record="true" was set
type Run struct {
	ID           string    `json:"id"`            // dir name; sortable lexicographically
	Scenario     string    `json:"scenario"`      // scenario name
	StartedAt    time.Time `json:"started_at"`
	StartedBy    string    `json:"started_by,omitempty"` // authenticated email from oauth2-proxy; empty when auth is off
	// Ports actually used for this run. Zero on older runs recorded
	// before this field existed — treat as "config default at the time".
	SIPPort      int       `json:"sip_port,omitempty"`
	RTPPortStart int       `json:"rtp_port_start,omitempty"`
	RTPPortEnd   int       `json:"rtp_port_end,omitempty"`
	FinishedAt   time.Time `json:"finished_at,omitempty"`
	Status       string    `json:"status"`        // running | done | error
	ExitCode     int       `json:"exit_code"`
	Error        string    `json:"error,omitempty"` // controller-side errors (non-zero exit, parse fail)

	// Aggregates parsed from results.json after the run finishes. Empty
	// while the run is in progress.
	Calls        []CallResult `json:"calls,omitempty"`
	Aggregate    Aggregate    `json:"aggregate,omitempty"`
}

// CallResult is one row out of voip_patrol's results.json. Field names
// match voip_patrol's JSON keys verbatim so the parser is a no-op cast.
type CallResult struct {
	Label             string    `json:"label"`
	Start             string    `json:"start"`
	End               string    `json:"end"`
	Action            string    `json:"action"`
	From              string    `json:"from"`
	To                string    `json:"to"`
	Result            string    `json:"result"`             // PASS | FAIL
	ExpectedCauseCode int       `json:"expected_cause_code"`
	CauseCode         int       `json:"cause_code"`
	Reason            string    `json:"reason"`
	CallID            string    `json:"callid"`
	Transport         string    `json:"transport"`
	PeerSocket        string    `json:"peer_socket"`
	Duration          int       `json:"duration"`
	MaxDuration       int       `json:"max_duration"`
	HangupDuration    int       `json:"hangup_duration"`
	SIPLatency        SIPLatency `json:"sip_latency"`
	RTPStats          []RTPStats `json:"rtp_stats"`
}

// SIPLatency mirrors voip_patrol's "sip_latency" sub-object.
type SIPLatency struct {
	Invite100Ms int `json:"invite100Ms"`
	Invite18xMs int `json:"invite18xMs"`
	Invite200Ms int `json:"invite200Ms"`
}

// RTPStats mirrors one entry of voip_patrol's "rtp_stats" array.
type RTPStats struct {
	RTT             int            `json:"rtt"`
	RemoteRTPSocket string         `json:"remote_rtp_socket"`
	CodecName       string         `json:"codec_name"`
	ClockRate       string         `json:"clock_rate"`
	Tx              RTPDirection   `json:"Tx"`
	Rx              RTPDirection   `json:"Rx"`
}

type RTPDirection struct {
	JitterAvg int     `json:"jitter_avg"`
	JitterMax int     `json:"jitter_max"`
	Pkt       int     `json:"pkt"`
	Kbytes    int     `json:"kbytes"`
	Loss      int     `json:"loss"`
	Discard   int     `json:"discard"`
	MosLQ     float64 `json:"mos_lq"`
}

// Aggregate summarizes a run across all calls. Computed after parsing.
type Aggregate struct {
	Total         int     `json:"total"`
	Pass          int     `json:"pass"`
	Fail          int     `json:"fail"`
	Invite200P50  int     `json:"invite_200_p50_ms"`
	Invite200P95  int     `json:"invite_200_p95_ms"`
	MOSAvgRx      float64 `json:"mos_avg_rx"`
	MOSAvgTx      float64 `json:"mos_avg_tx"`
	RTTAvgMs      int     `json:"rtt_avg_ms"`
	PacketsLossTx int     `json:"packets_loss_tx"`
}

// NewRun mints a new Run with a sortable ID and a fresh output dir.
// The dir is created on disk; callers can write files into Dir(runsRoot).
func NewRun(runsRoot, scenario string) (*Run, error) {
	now := time.Now().UTC()
	id := fmt.Sprintf("%s-%s", now.Format("20060102-150405"), scenario)
	r := &Run{
		ID:        id,
		Scenario:  scenario,
		StartedAt: now,
		Status:    "running",
	}
	if err := os.MkdirAll(r.Dir(runsRoot), 0o755); err != nil {
		return nil, err
	}
	return r, nil
}

// Dir is the absolute path to this run's output directory.
func (r *Run) Dir(runsRoot string) string {
	return filepath.Join(runsRoot, r.ID)
}

// Save serializes the run metadata to run.json in its dir.
func (r *Run) Save(runsRoot string) error {
	f, err := os.Create(filepath.Join(r.Dir(runsRoot), "run.json"))
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// LoadRun reads one run by ID.
func LoadRun(runsRoot, id string) (*Run, error) {
	f, err := os.Open(filepath.Join(runsRoot, id, "run.json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r Run
	if err := json.NewDecoder(f).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListRuns returns every run, newest first. Missing run.json files
// are skipped (probably an in-progress run that hasn't saved yet, or a
// hand-deleted record).
func ListRuns(runsRoot string) ([]Run, error) {
	entries, err := os.ReadDir(runsRoot)
	if err != nil {
		return nil, err
	}
	var out []Run
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		r, err := LoadRun(runsRoot, e.Name())
		if err != nil {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// WAVFiles returns the recorded WAV file basenames for this run, sorted.
// Empty when record="false" or the run hasn't produced any yet.
func (r *Run) WAVFiles(runsRoot string) []string {
	entries, err := os.ReadDir(r.Dir(runsRoot))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".wav") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}
