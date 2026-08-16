package controller

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/jchavanton/ace/models"
)

// Firewall programs the host's iptables filter table with the operator's
// ordered rule list. Rules land in a dedicated chain (ACE-FIREWALL)
// that ACE owns exclusively — we flush and rewrite it on every Apply.
// The chain is hooked into INPUT once (idempotent); ACE never touches
// other rules in INPUT.
//
// Each rule has an explicit Action (ACCEPT or DROP). iptables evaluates
// top-to-bottom, first match wins. Typical layout for a SIP guard:
// ACCEPT rows for the allow-list first, then a catch-all DROP row for
// the protected port range. Rows below a broad DROP are unreachable —
// that's the tradeoff for having full control.
//
// An empty rule set is a no-op: the chain is empty, execution falls
// through the implicit RETURN to whatever else is in INPUT.
//
// Requires the iptables binary and NET_ADMIN in the container. Apply
// returns a clear error when either is missing.
type Firewall struct {
	StateDir     string // where firewall.json lives
	Chain        string // e.g. "ACE-FIREWALL"
	IptablesPath string // typically "iptables"; "" = look up in PATH
	SavePath     string // "" = don't persist across reboot; typically "/etc/iptables/rules.v4"

	mu sync.Mutex
}

// NewFirewall returns a Firewall wired to the given state dir. Callers
// don't need to check whether iptables is available — Apply reports it.
func NewFirewall(stateDir string) *Firewall {
	return &Firewall{
		StateDir:     stateDir,
		Chain:        "ACE-FIREWALL",
		IptablesPath: "iptables",
	}
}

// Load returns the persisted rule set, or an empty config if the file
// doesn't exist. Thin wrapper around models — keeps handlers from
// touching two packages for the load path.
func (f *Firewall) Load() (models.FirewallConfig, error) {
	return models.LoadFirewallConfig(f.StateDir)
}

// SaveAndApply persists the rule set and rewrites the iptables chain to
// match. Callers are expected to have Validate()'d each rule; SaveAndApply
// re-validates defensively.
//
// On any iptables error the JSON file is still saved — the operator can
// then fix the environment (missing binary, missing capability) and
// re-Apply without losing rules. This matches the "save + apply" UX:
// save is durable; apply may fail loudly.
func (f *Firewall) SaveAndApply(cfg models.FirewallConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i := range cfg.Rules {
		if err := cfg.Rules[i].Validate(); err != nil {
			return fmt.Errorf("rule %d: %w", i+1, err)
		}
	}
	if err := models.SaveFirewallConfig(f.StateDir, cfg); err != nil {
		return fmt.Errorf("save firewall.json: %w", err)
	}
	return f.applyLocked(cfg)
}

// ApplyPersisted loads firewall.json and applies it. Called once at
// startup so a container restart doesn't lose the rules that were
// active before the restart (iptables state itself is host-owned; on
// GCP the VM reboot flushes it).
//
// Missing/empty file is a no-op success — the chain gets created empty,
// which matches "no rules yet."
func (f *Firewall) ApplyPersisted() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cfg, err := models.LoadFirewallConfig(f.StateDir)
	if err != nil {
		return err
	}
	return f.applyLocked(cfg)
}

// applyLocked does the actual iptables work. Caller holds f.mu.
//
// The sequence is:
//  1. Ensure the chain exists (`-N`), swallowing "chain already exists".
//  2. Ensure INPUT has exactly one jump to it (idempotent add).
//  3. Build a restore payload that flushes + repopulates just the chain,
//     and pipe it into `iptables-restore --noflush -T filter`.
//
// Step 3 is atomic from the kernel's perspective — no window where the
// chain is empty while rules are being reinserted.
func (f *Firewall) applyLocked(cfg models.FirewallConfig) error {
	ipt := f.IptablesPath
	if ipt == "" {
		ipt = "iptables"
	}
	if _, err := exec.LookPath(ipt); err != nil {
		return fmt.Errorf("iptables not found in PATH: %w", err)
	}

	// -N is the idempotent-ish create. iptables returns non-zero + a
	// specific message when the chain already exists; that's fine.
	if out, err := run(ipt, "-N", f.Chain); err != nil {
		if !strings.Contains(string(out), "already exists") {
			return fmt.Errorf("create chain %s: %v (%s)", f.Chain, err, strings.TrimSpace(string(out)))
		}
	}

	// Ensure INPUT jumps to our chain. -C returns 0 if the rule exists.
	if _, err := run(ipt, "-C", "INPUT", "-j", f.Chain); err != nil {
		if out, err := run(ipt, "-I", "INPUT", "1", "-j", f.Chain); err != nil {
			return fmt.Errorf("hook INPUT -> %s: %v (%s)", f.Chain, err, strings.TrimSpace(string(out)))
		}
	}

	// Build the restore payload. --noflush leaves other tables/chains
	// untouched; the `:CHAIN - [0:0]` line resets just this chain.
	payload := buildRestorePayload(f.Chain, cfg.Rules)

	cmd := exec.Command(ipt+"-restore", "--noflush", "-T", "filter")
	cmd.Stdin = strings.NewReader(payload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("iptables-restore: %v\npayload:\n%s\nstderr:\n%s",
			err, payload, stderr.String())
	}

	if f.SavePath != "" {
		_ = persistRules(ipt+"-save", f.SavePath)
	}
	return nil
}

// buildRestorePayload emits the text an `iptables-restore --noflush -T
// filter` call will accept: reset the chain, then all ACCEPT rules in
// operator order, then all DROP rules in operator order, then COMMIT.
//
// The ACCEPT-before-DROP sort matches the "SIP guard" mental model
// (allow a few sources, block everything else) and prevents the common
// footgun of a broad DROP row above a specific ACCEPT making the
// ACCEPT unreachable. It also means you can't express "ACCEPT this
// subnet except for one host" in a single chain — accept that; that
// use case is rare enough to route around by writing rules against
// specific ports/protocols instead.
//
// Rules are stored in operator order on disk (firewall.json) so the UI
// still shows what they typed. Only the emitted chain is reordered.
func buildRestorePayload(chain string, rules []models.FirewallRule) string {
	var b strings.Builder
	b.WriteString("*filter\n")
	fmt.Fprintf(&b, ":%s - [0:0]\n", chain)
	for _, r := range rules {
		if r.Action != "DROP" {
			fmt.Fprintf(&b, "-A %s%s\n", chain, ruleArgs(r))
		}
	}
	for _, r := range rules {
		if r.Action == "DROP" {
			fmt.Fprintf(&b, "-A %s%s\n", chain, ruleArgs(r))
		}
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

// ruleArgs turns a FirewallRule into the iptables argument tail
// (starting with a leading space). --dport requires -p tcp|udp; the
// validator enforces that pairing. r.Action is assumed already
// normalized to ACCEPT or DROP by Validate().
func ruleArgs(r models.FirewallRule) string {
	var b strings.Builder
	if r.CIDR != "" {
		fmt.Fprintf(&b, " -s %s", r.CIDR)
	}
	if r.Interface != "" {
		fmt.Fprintf(&b, " -i %s", r.Interface)
	}
	if r.Transport != "" {
		fmt.Fprintf(&b, " -p %s", r.Transport)
	}
	if r.Port != "" {
		port := strings.ReplaceAll(r.Port, "-", ":")
		fmt.Fprintf(&b, " --dport %s", port)
	}
	if r.Comment != "" {
		fmt.Fprintf(&b, ` -m comment --comment "%s"`, r.Comment)
	}
	action := r.Action
	if action == "" {
		action = "ACCEPT"
	}
	fmt.Fprintf(&b, " -j %s", action)
	return b.String()
}

func run(bin string, args ...string) ([]byte, error) {
	return exec.Command(bin, args...).CombinedOutput()
}

// persistRules dumps the current iptables state to savePath so a reboot
// (or iptables-persistent) can restore it. Best-effort; caller ignores
// the error.
func persistRules(saveBin, savePath string) error {
	out, err := exec.Command(saveBin).Output()
	if err != nil {
		return err
	}
	tmp := savePath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, savePath)
}

// InterfaceNames returns the host's UP, non-loopback interface names,
// sorted. Used by the UI to populate the per-rule interface dropdown.
// Empty on lookup failure — the UI shows only "any" in that case.
func InterfaceNames() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		out = append(out, ifc.Name)
	}
	sort.Strings(out)
	return out
}
