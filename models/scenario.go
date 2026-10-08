// Package models holds the in-memory representations of scenarios and
// run records. Both are persisted to the filesystem (XML for scenarios,
// JSON for run records); the structs are what handlers and templates
// consume.
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

// Scenario is a single voip_patrol XML scenario the controller can run.
// In v1 the controller treats the XML as opaque — load, run, list — and
// doesn't parse it; later versions may add a typed view of the call
// action's attributes (callee, repeat, hangup, ...) for UI editing.
type Scenario struct {
	// Name is the filename without the .xml extension. Used as the URL
	// slug, as the run output dir prefix, and in UI listings.
	Name string

	// Path is the absolute path to the .xml file on disk.
	Path string

	// SizeBytes is from the file's stat, shown in the listing.
	SizeBytes int64

	// ModTime is from the file's stat, shown in the listing.
	ModTime time.Time

	// Ports holds the per-scenario saved SIP + RTP defaults, loaded
	// from the sidecar file `<name>.ports.json`. Zero fields mean
	// "not saved — fall back to the runner's global defaults." The
	// sidecar is opt-in: absent file = zero Ports.
	Ports ScenarioPorts

	// Verdict is the per-scenario pass/fail criteria applied by ACE
	// after voip_patrol finishes, read from `<name>.verdict.json`.
	// Zero fields = no extra checks (ACE trusts voip_patrol's verdict
	// as-is); absent file = zero Verdict.
	Verdict ScenarioVerdict
}

// ScenarioPorts is the on-disk shape of a scenario's saved per-run
// preferences. Despite the name it also carries the timeout override —
// keeping one sidecar file (`<name>.ports.json`) avoids a second load
// path and a migration for existing scenarios. Zero fields = unset, so
// callers can treat a missing file as an empty struct.
//
// TimeoutSeconds: 0 = "not set, use runner default". A saved value of
// -1 means "unlimited" (no per-run deadline). Positive = seconds.
type ScenarioPorts struct {
	SIP            int    `json:"sip,omitempty"`
	RTPPortStart   int    `json:"rtp_port_start,omitempty"`
	RTPPortEnd     int    `json:"rtp_port_end,omitempty"`
	PublicAddress  string `json:"public_address,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	// Transport restricts which SIP transport voip_patrol listens on.
	// "" = default (mixed — scenario XML picks per-action), "udp" adds
	// --udp, "tcp" adds --tcp. "tls" is a saved preference for the UI
	// but passes no CLI flag: TLS is enabled per-action in the scenario
	// XML, not toggled at the process level.
	Transport string `json:"transport,omitempty"`
	// Nameservers is a comma-separated list of DNS servers voip_patrol
	// uses for SIP SRV/NAPTR resolution — one --nameserver arg per
	// entry on the CLI. Empty = don't pass any --nameserver, and
	// voip_patrol falls back to the host's resolver.
	Nameservers string `json:"nameservers,omitempty"`
}

// PortsPath returns the absolute path to the scenario's sidecar
// ports JSON file.
func (s *Scenario) PortsPath() string {
	return strings.TrimSuffix(s.Path, ".xml") + ".ports.json"
}

// loadScenarioPorts reads the sidecar for the given scenario XML path.
// Missing file returns a zero ScenarioPorts + nil (that's the normal
// "no saved ports" case). Malformed JSON returns an error.
func loadScenarioPorts(scenarioPath string) (ScenarioPorts, error) {
	p := strings.TrimSuffix(scenarioPath, ".xml") + ".ports.json"
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ScenarioPorts{}, nil
		}
		return ScenarioPorts{}, err
	}
	defer f.Close()
	var out ScenarioPorts
	if err := json.NewDecoder(f).Decode(&out); err != nil {
		return ScenarioPorts{}, fmt.Errorf("parse %s: %w", p, err)
	}
	return out, nil
}

// SaveScenarioPorts writes the sidecar for scenario `name` under
// dir. Zero-fielded ScenarioPorts still writes {} — call
// DeleteScenarioPorts to unset instead.
func SaveScenarioPorts(dir, name string, p ScenarioPorts) error {
	path := filepath.Join(dir, name+".ports.json")
	tmp, err := os.CreateTemp(dir, "."+name+".ports.*.tmp")
	if err != nil {
		return err
	}
	// Best-effort cleanup of the tmp file if we bail before rename.
	defer os.Remove(tmp.Name())
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(p); err != nil {
		tmp.Close()
		return err
	}
	// CreateTemp makes the file 0600; keep it readable like the scenario XML.
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Atomic replace so a torn write doesn't leave an unreadable file.
	return os.Rename(tmp.Name(), path)
}

// DeleteScenarioPorts removes the sidecar for scenario `name`.
// ENOENT is not an error — the caller wanted it gone either way.
func DeleteScenarioPorts(dir, name string) error {
	path := filepath.Join(dir, name+".ports.json")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ScenarioVerdict is ACE's post-run pass/fail criteria for a scenario,
// persisted alongside the XML as `<name>.verdict.json`. Verdict rules
// run after voip_patrol finishes and read fields it emitted (currently
// just rtp_stats[].{Tx,Rx}.voice_frames when energy_stats="true").
//
// Each min_ field is 0 = "disabled, no check." A non-zero value is the
// minimum voice duration in milliseconds that must have been sampled
// in the given direction by each call in the run. ms is converted to a
// frame count at check time using SamplerPeriodMs — keeping the config
// in ms means a cadence change in voip_patrol doesn't retroactively
// invalidate stored thresholds.
// Any shortfall downgrades the call's result from PASS to FAIL and
// appends the shortfall to its reason.
type ScenarioVerdict struct {
	MinRxVoiceMs int `json:"min_rx_voice_ms,omitempty"`
	MinTxVoiceMs int `json:"min_tx_voice_ms,omitempty"`
	// Level thresholds operate on voip_patrol's 0..255 mu-law scale
	// (see levelToDBov below). All six are 0 = disabled.
	// Min floors: direction's lowest cross-stream value must be at
	// least this. Max ceilings: direction's highest cross-stream
	// value must be at most this (clipping guard).
	MinRxLevelAvg  int `json:"min_rx_level_avg,omitempty"`
	MinTxLevelAvg  int `json:"min_tx_level_avg,omitempty"`
	MinRxLevelPeak int `json:"min_rx_level_peak,omitempty"`
	MinTxLevelPeak int `json:"min_tx_level_peak,omitempty"`
	MaxRxLevelPeak int `json:"max_rx_level_peak,omitempty"`
	MaxTxLevelPeak int `json:"max_tx_level_peak,omitempty"`
}

// SamplerPeriodMs is voip_patrol's energy sampler tick period
// (voip_patrol/src/voip_patrol/action.cc, do_wait loop). Verdict ms
// thresholds divide by this to get a frame count. If the voip_patrol
// cadence ever changes, update this constant in lockstep.
const SamplerPeriodMs = 100

// levelToDBov converts a 0..255 mu-law signal level (as emitted by
// pjsua_conf_get_signal_level) to approximate dBov so error messages
// can show both numbers. Table is pre-computed from the mu-law curve
// matching the dBov table in voip_patrol's README; linear interpolation
// between stops is close enough for an operator-facing diagnostic.
// Returns 0 at full scale and more-negative values as the signal gets
// quieter. Level 0 (digital silence) returns -99 as a sentinel floor.
func levelToDBov(level int) int {
	if level <= 0 {
		return -99
	}
	if level >= 255 {
		return 0
	}
	// (level, dBov) stops, keyed on the README table.
	stops := [...]struct{ L, D int }{
		{8, -64}, {16, -56}, {32, -48}, {64, -39}, {96, -31},
		{120, -26}, {128, -25}, {160, -18}, {192, -12}, {224, -6}, {255, 0},
	}
	// Below the lowest stop: linear from (0,-99) to (8,-64).
	if level < stops[0].L {
		return -99 + (level * (stops[0].D - (-99)) / stops[0].L)
	}
	for i := 0; i < len(stops)-1; i++ {
		a, b := stops[i], stops[i+1]
		if level >= a.L && level <= b.L {
			span := b.L - a.L
			if span == 0 {
				return a.D
			}
			return a.D + ((level-a.L)*(b.D-a.D))/span
		}
	}
	return 0
}

// IsZero returns true when no checks are configured; callers skip
// loading/applying the verdict entirely in that case.
func (v ScenarioVerdict) IsZero() bool { return v == (ScenarioVerdict{}) }

// VerdictPath returns the absolute path to the scenario's sidecar
// verdict JSON file.
func (s *Scenario) VerdictPath() string {
	return strings.TrimSuffix(s.Path, ".xml") + ".verdict.json"
}

// loadScenarioVerdict reads the sidecar for the given scenario XML path.
// Missing file returns a zero ScenarioVerdict + nil (the normal "no
// extra checks" case). Malformed JSON returns an error.
func loadScenarioVerdict(scenarioPath string) (ScenarioVerdict, error) {
	p := strings.TrimSuffix(scenarioPath, ".xml") + ".verdict.json"
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ScenarioVerdict{}, nil
		}
		return ScenarioVerdict{}, err
	}
	defer f.Close()
	var out ScenarioVerdict
	if err := json.NewDecoder(f).Decode(&out); err != nil {
		return ScenarioVerdict{}, fmt.Errorf("parse %s: %w", p, err)
	}
	return out, nil
}

// LoadScenarioVerdict is the public counterpart used by the runner,
// which doesn't carry a full Scenario — just the dir + name.
func LoadScenarioVerdict(dir, name string) (ScenarioVerdict, error) {
	return loadScenarioVerdict(filepath.Join(dir, name+".xml"))
}

// SaveScenarioVerdict writes the sidecar for scenario `name` atomically.
// Caller should DeleteScenarioVerdict instead of saving an all-zero
// struct, so a disabled verdict leaves no file on disk.
func SaveScenarioVerdict(dir, name string, v ScenarioVerdict) error {
	path := filepath.Join(dir, name+".verdict.json")
	tmp, err := os.CreateTemp(dir, "."+name+".verdict.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		tmp.Close()
		return err
	}
	// CreateTemp makes the file 0600; keep it readable like the scenario XML.
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// DeleteScenarioVerdict removes the sidecar. ENOENT is not an error.
func DeleteScenarioVerdict(dir, name string) error {
	path := filepath.Join(dir, name+".verdict.json")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Apply enforces the verdict against one CallResult. Returns (pass,
// reason-suffix). pass=false means the caller should downgrade the
// call's Result to FAIL; reason-suffix is "" when there is nothing
// to append (either the check passed or wasn't configured).
//
// Semantics:
//   - Zero thresholds = no check; returns (true, "").
//   - Non-zero threshold on a call with no rtp_stats = fail (sampler
//     never ran, which is the whole point of configuring the verdict).
//   - Thresholds are compared against the call's last rtp_stats block,
//     which holds the whole call's totals.
func (v ScenarioVerdict) Apply(call *CallResult) (bool, string) {
	if v.IsZero() {
		return true, ""
	}
	// Only call-type actions have RTP; message/register/etc. are
	// opaque to the verdict (and shouldn't configure it anyway).
	if call.Action != "call" && call.Action != "accept" {
		return true, ""
	}
	if len(call.RTPStats) == 0 {
		return false, "no rtp_stats produced; is energy_stats=\"true\" set on the action?"
	}
	// Peak is the call's running max, avg its call-wide mean (see EnergyStats).
	last := call.EnergyStats()
	rx, tx := last.Rx, last.Tx
	rxMs := rx.VoiceFrames * SamplerPeriodMs
	txMs := tx.VoiceFrames * SamplerPeriodMs
	var reasons []string
	if v.MinRxVoiceMs > 0 && rxMs < v.MinRxVoiceMs {
		reasons = append(reasons, fmt.Sprintf("rx voice=%dms (%d frames) below min=%dms", rxMs, rx.VoiceFrames, v.MinRxVoiceMs))
	}
	if v.MinTxVoiceMs > 0 && txMs < v.MinTxVoiceMs {
		reasons = append(reasons, fmt.Sprintf("tx voice=%dms (%d frames) below min=%dms", txMs, tx.VoiceFrames, v.MinTxVoiceMs))
	}
	if v.MinRxLevelAvg > 0 && rx.LevelAvg < v.MinRxLevelAvg {
		reasons = append(reasons, fmt.Sprintf("rx level_avg=%d below min=%d (%d vs %d dBov)",
			rx.LevelAvg, v.MinRxLevelAvg, levelToDBov(rx.LevelAvg), levelToDBov(v.MinRxLevelAvg)))
	}
	if v.MinTxLevelAvg > 0 && tx.LevelAvg < v.MinTxLevelAvg {
		reasons = append(reasons, fmt.Sprintf("tx level_avg=%d below min=%d (%d vs %d dBov)",
			tx.LevelAvg, v.MinTxLevelAvg, levelToDBov(tx.LevelAvg), levelToDBov(v.MinTxLevelAvg)))
	}
	if v.MinRxLevelPeak > 0 && rx.LevelPeak < v.MinRxLevelPeak {
		reasons = append(reasons, fmt.Sprintf("rx level_peak=%d below min=%d (%d vs %d dBov)",
			rx.LevelPeak, v.MinRxLevelPeak, levelToDBov(rx.LevelPeak), levelToDBov(v.MinRxLevelPeak)))
	}
	if v.MinTxLevelPeak > 0 && tx.LevelPeak < v.MinTxLevelPeak {
		reasons = append(reasons, fmt.Sprintf("tx level_peak=%d below min=%d (%d vs %d dBov)",
			tx.LevelPeak, v.MinTxLevelPeak, levelToDBov(tx.LevelPeak), levelToDBov(v.MinTxLevelPeak)))
	}
	if v.MaxRxLevelPeak > 0 && rx.LevelPeak > v.MaxRxLevelPeak {
		reasons = append(reasons, fmt.Sprintf("rx level_peak=%d above max=%d (%d vs %d dBov, clipping)",
			rx.LevelPeak, v.MaxRxLevelPeak, levelToDBov(rx.LevelPeak), levelToDBov(v.MaxRxLevelPeak)))
	}
	if v.MaxTxLevelPeak > 0 && tx.LevelPeak > v.MaxTxLevelPeak {
		reasons = append(reasons, fmt.Sprintf("tx level_peak=%d above max=%d (%d vs %d dBov, clipping)",
			tx.LevelPeak, v.MaxTxLevelPeak, levelToDBov(tx.LevelPeak), levelToDBov(v.MaxTxLevelPeak)))
	}
	if len(reasons) == 0 {
		return true, ""
	}
	return false, strings.Join(reasons, "; ")
}

// LoadScenarios returns every scenario in dir, sorted by name. Non-XML
// files are skipped. Returns an empty slice (not an error) when dir is
// empty — that's the fresh-install state and the UI handles it.
func LoadScenarios(dir string) ([]Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Scenario
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".xml") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(dir, e.Name())
		// Sidecar load errors are non-fatal for the list view — a
		// broken JSON just means "no saved ports for this scenario."
		// The save handler validates before writing, so this should
		// be rare, and a whole-list failure would be worse UX.
		ports, _ := loadScenarioPorts(p)
		verdict, _ := loadScenarioVerdict(p)
		out = append(out, Scenario{
			Name:      strings.TrimSuffix(e.Name(), ".xml"),
			Path:      p,
			SizeBytes: info.Size(),
			ModTime:   info.ModTime(),
			Ports:     ports,
			Verdict:   verdict,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// LoadScenario fetches one by name; returns os.ErrNotExist if absent.
// Sidecar port file is loaded best-effort — a missing sidecar is the
// normal "no saved ports" case, a broken sidecar leaves Ports zero.
func LoadScenario(dir, name string) (*Scenario, error) {
	path := filepath.Join(dir, name+".xml")
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	ports, _ := loadScenarioPorts(path)
	verdict, _ := loadScenarioVerdict(path)
	return &Scenario{
		Name:      name,
		Path:      path,
		SizeBytes: info.Size(),
		ModTime:   info.ModTime(),
		Ports:     ports,
		Verdict:   verdict,
	}, nil
}

// ReadXML reads the raw scenario XML for display / editing.
func (s *Scenario) ReadXML() (string, error) {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
