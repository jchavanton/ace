// Package models holds the in-memory representations of scenarios and
// run records. Both are persisted to the filesystem (XML for scenarios,
// JSON for run records); the structs are what handlers and templates
// consume.
package models

import (
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
		out = append(out, Scenario{
			Name:      strings.TrimSuffix(e.Name(), ".xml"),
			Path:      filepath.Join(dir, e.Name()),
			SizeBytes: info.Size(),
			ModTime:   info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// LoadScenario fetches one by name; returns os.ErrNotExist if absent.
func LoadScenario(dir, name string) (*Scenario, error) {
	path := filepath.Join(dir, name+".xml")
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return &Scenario{
		Name:      name,
		Path:      path,
		SizeBytes: info.Size(),
		ModTime:   info.ModTime(),
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
