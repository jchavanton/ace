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
}

// ScenarioPorts is the on-disk shape of a scenario's saved port
// preferences. Serialized JSON to `<name>.ports.json` next to the
// scenario XML. Zero fields = unset, so callers can treat a missing
// file as an empty struct.
type ScenarioPorts struct {
	SIP           int    `json:"sip,omitempty"`
	RTPPortStart  int    `json:"rtp_port_start,omitempty"`
	RTPPortEnd    int    `json:"rtp_port_end,omitempty"`
	PublicAddress string `json:"public_address,omitempty"`
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
		out = append(out, Scenario{
			Name:      strings.TrimSuffix(e.Name(), ".xml"),
			Path:      p,
			SizeBytes: info.Size(),
			ModTime:   info.ModTime(),
			Ports:     ports,
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
	return &Scenario{
		Name:      name,
		Path:      path,
		SizeBytes: info.Size(),
		ModTime:   info.ModTime(),
		Ports:     ports,
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
