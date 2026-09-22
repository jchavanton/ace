package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Bot is a scheduled recurring execution of a scenario. Bots are
// persisted as JSON files under $BotsDir/<name>.json. When a scheduled
// run finishes with any FAIL call or a non-done status, the scheduler
// appends an Alert record so the operator sees a red badge in /bots.
type Bot struct {
	// Name is the URL slug + JSON filename basename. Constrained to
	// letters, digits, '_' and '-' by the handler.
	Name string `json:"name"`

	// Scenario is the scenario file (without .xml) this bot runs.
	Scenario string `json:"scenario"`

	// IntervalMinutes is how often the bot triggers. Must be >= 1.
	IntervalMinutes int `json:"interval_minutes"`

	// Enabled = false pauses the schedule without deleting the bot.
	Enabled bool `json:"enabled"`

	// LastRunID / LastStatus reflect the most recent triggered run, so
	// the list view can show status without reloading every run.json.
	LastRunID  string    `json:"last_run_id,omitempty"`
	LastStatus string    `json:"last_status,omitempty"` // done | error | stopped | fail
	LastRunAt  time.Time `json:"last_run_at,omitempty"`

	// NextRunAt is the wall-clock time the scheduler will next consider
	// this bot for firing. Advanced after each trigger.
	NextRunAt time.Time `json:"next_run_at,omitempty"`
}

// Alert is one failure record. Kept as a single append-only file
// ($BotsDir/alerts.json, JSON-per-line) rather than per-bot files so
// the "recent alerts" view is a single read.
type Alert struct {
	ID       string    `json:"id"` // sortable: RFC3339Nano-<bot>
	Bot      string    `json:"bot"`
	Scenario string    `json:"scenario"`
	RunID    string    `json:"run_id"`
	At       time.Time `json:"at"`
	Reason   string    `json:"reason"` // "FAIL: 2/5", "error: voip_patrol crashed", ...
	Acked    bool      `json:"acked,omitempty"`
}

// SaveBot writes the bot to $dir/<name>.json atomically.
func SaveBot(dir string, b *Bot) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, b.Name+".json")
	tmp, err := os.CreateTemp(dir, "."+b.Name+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadBot reads one bot by name.
func LoadBot(dir, name string) (*Bot, error) {
	f, err := os.Open(filepath.Join(dir, name+".json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var b Bot
	if err := json.NewDecoder(f).Decode(&b); err != nil {
		return nil, err
	}
	return &b, nil
}

// ListBots returns every bot in dir, sorted by name. Missing dir =
// empty slice (fresh install has no bots).
func ListBots(dir string) ([]Bot, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Bot
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		b, err := LoadBot(dir, name)
		if err != nil {
			continue
		}
		// Sibling files in this dir (alerts.json, alert_config.json,
		// alert_state.json) parse as a Bot with a zero Name. Positive
		// filter: a real bot has Name matching the filename.
		if b.Name == "" || b.Name != name {
			continue
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteBot removes a bot file. ENOENT is not an error.
func DeleteBot(dir, name string) error {
	path := filepath.Join(dir, name+".json")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// AppendAlert appends one alert record to $dir/alerts.json (JSON per
// line — same shape as voip_patrol's results.json, so the reader is
// symmetrical).
func AppendAlert(dir string, a Alert) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "alerts.json")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// ListAlerts reads alerts.json. Returns newest first. Missing file =
// empty slice. Malformed lines are skipped rather than failing the
// load, matching the voip_patrol results.json parser.
func ListAlerts(dir string) ([]Alert, error) {
	path := filepath.Join(dir, "alerts.json")
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var out []Alert
	for {
		var a Alert
		if err := dec.Decode(&a); err != nil {
			break
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
}

// AckAlert flips the Acked flag on the alert with the matching ID and
// rewrites alerts.json. Not found = no-op (idempotent from the UI's
// perspective).
func AckAlert(dir, id string) error {
	alerts, err := ListAlerts(dir)
	if err != nil {
		return err
	}
	changed := false
	for i := range alerts {
		if alerts[i].ID == id && !alerts[i].Acked {
			alerts[i].Acked = true
			changed = true
		}
	}
	if !changed {
		return nil
	}
	// Preserve original (chronological) order in the file so appending
	// stays cheap; ListAlerts sorts on read.
	sort.Slice(alerts, func(i, j int) bool { return alerts[i].At.Before(alerts[j].At) })
	path := filepath.Join(dir, "alerts.json")
	tmp, err := os.CreateTemp(dir, ".alerts.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	enc := json.NewEncoder(tmp)
	for _, a := range alerts {
		if err := enc.Encode(a); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// UnackedAlertCount returns the number of alerts with Acked=false. Used
// by the navbar badge; a full ListAlerts + count is fine (alerts.json
// is bounded — an operator will ack or the file will be pruned).
func UnackedAlertCount(dir string) int {
	alerts, _ := ListAlerts(dir)
	n := 0
	for _, a := range alerts {
		if !a.Acked {
			n++
		}
	}
	return n
}

// NewAlertID mints a sortable alert ID. Two alerts at the same instant
// for the same bot are indistinguishable — acceptable, alerts aren't
// keyed by anything downstream.
func NewAlertID(at time.Time, bot string) string {
	return fmt.Sprintf("%s-%s", at.UTC().Format("20060102T150405.000000000Z"), bot)
}
