package models

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// AlertConfig is the global email-delivery config for bot alerts. One
// file per install ($BotsDir/alert_config.json). Empty SMTPHost keeps
// the pre-email behavior: alerts still get appended to alerts.json and
// shown in the UI, but no email is sent.
//
// Reminder logic is optional: 0 (default) fires a single email on the
// ok → firing transition and nothing further while the bot stays
// firing; nonzero re-emails every N hours until the bot recovers,
// matching sip-proxy's cadence.
type AlertConfig struct {
	SMTPHost              string   `json:"smtp_host,omitempty"`
	SMTPPort              int      `json:"smtp_port,omitempty"`
	SMTPUsername          string   `json:"smtp_username,omitempty"`
	SMTPPassword          string   `json:"smtp_password,omitempty"`
	FromAddress           string   `json:"from_address,omitempty"`
	Recipients            []string `json:"recipients,omitempty"`
	ReminderIntervalHours int      `json:"reminder_interval_hours,omitempty"`

	// Cleanup knobs. Both zero on a fresh install → the controller
	// applies the flag defaults (30 days / 500 alerts). Persisting them
	// here lets the operator edit from the UI without a redeploy.
	// Explicit -1 means "disable that dimension" (kept in the file so
	// the UI can distinguish "unset" from "disabled").
	RunsRetentionDays    int       `json:"runs_retention_days,omitempty"`
	AlertsRetentionCount int       `json:"alerts_retention_count,omitempty"`
	LastCleanupAt        time.Time `json:"last_cleanup_at,omitempty"`
	LastCleanupSummary   string    `json:"last_cleanup_summary,omitempty"`
}

// AlertState is one bot's slot in the state machine. Persisted as one
// row inside alert_state.json (a map keyed by bot name). Missing entry
// = state "ok" with zero fields.
//
// Consecutive counts are informational — we fire on any failure per
// spec, so ConsecutiveFails is only surfaced in the UI/email body.
type AlertState struct {
	State            string    `json:"state"` // "ok" | "firing"
	LastAt           time.Time `json:"last_at,omitempty"`
	LastNotifiedAt   time.Time `json:"last_notified_at,omitempty"`
	LastRunID        string    `json:"last_run_id,omitempty"`
	LastReason       string    `json:"last_reason,omitempty"`
	ConsecutiveFails int       `json:"consecutive_fails,omitempty"`
}

// LoadAlertConfig reads $dir/alert_config.json. Missing file returns a
// zero AlertConfig — that's the "no email configured" state and the
// UI shows it as such.
func LoadAlertConfig(dir string) (AlertConfig, error) {
	path := filepath.Join(dir, "alert_config.json")
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AlertConfig{}, nil
		}
		return AlertConfig{}, err
	}
	defer f.Close()
	var c AlertConfig
	if err := json.NewDecoder(f).Decode(&c); err != nil {
		return AlertConfig{}, err
	}
	return c, nil
}

// SaveAlertConfig writes $dir/alert_config.json atomically.
func SaveAlertConfig(dir string, c AlertConfig) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "alert_config.json")
	tmp, err := os.CreateTemp(dir, ".alert_config.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadAlertStates reads $dir/alert_state.json. Missing file returns an
// empty map (fresh install, nothing firing yet).
func LoadAlertStates(dir string) (map[string]AlertState, error) {
	path := filepath.Join(dir, "alert_state.json")
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]AlertState{}, nil
		}
		return nil, err
	}
	defer f.Close()
	out := map[string]AlertState{}
	if err := json.NewDecoder(f).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// SaveAlertStates writes the whole state map atomically. Callers hold
// no lock — writes are serialized by the scheduler's single goroutine,
// and the UI reads a snapshot (no shared struct).
func SaveAlertStates(dir string, states map[string]AlertState) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "alert_state.json")
	tmp, err := os.CreateTemp(dir, ".alert_state.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(states); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// FiringBots returns the names of bots currently in "firing" state,
// sorted for stable UI ordering.
func FiringBots(dir string) ([]string, error) {
	states, err := LoadAlertStates(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for name, st := range states {
		if st.State == "firing" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}
