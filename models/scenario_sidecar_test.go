package models

import (
	"os"
	"path/filepath"
	"testing"
)

// The scenario XML is readable from the host; its sidecars should be too.
func TestScenarioSidecarsAreWorldReadable(t *testing.T) {
	dir := t.TempDir()
	if err := SaveScenarioPorts(dir, "probe", ScenarioPorts{SIP: 5070}); err != nil {
		t.Fatal(err)
	}
	if err := SaveScenarioVerdict(dir, "probe", ScenarioVerdict{MinRxVoiceMs: 1000}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"probe.ports.json", "probe.verdict.json"} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o044 != 0o044 {
			t.Errorf("%s mode %v, want group/other readable like the scenario XML", f, fi.Mode().Perm())
		}
	}
}
