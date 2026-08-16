package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FirewallRule is one entry in the operator's ordered rule list.
// Rows are evaluated top-to-bottom; the first match wins. That means
// a typical "allow-list + guard" setup is ACCEPT rows first, DROP rows
// last. Misses fall through the chain (implicit RETURN) to whatever
// else is in INPUT.
//
// Fields map 1:1 to iptables flags:
//
//	CIDR       -> -s <cidr>            (source; required)
//	Interface  -> -i <iface>           (input interface; optional, "" = any)
//	Transport  -> -p tcp|udp           (optional, "" = any protocol)
//	Port       -> --dport N or N:M     (optional; requires Transport when set)
//	Action     -> -j ACCEPT|DROP       ("" defaults to ACCEPT for backward compat)
//	Comment    -> -m comment --comment (optional; sanitized)
type FirewallRule struct {
	CIDR      string `json:"cidr"`
	Interface string `json:"interface,omitempty"`
	Transport string `json:"transport,omitempty"` // "" | "tcp" | "udp"
	Port      string `json:"port,omitempty"`      // "5093" or "4000-14000"
	Action    string `json:"action,omitempty"`    // "" | "ACCEPT" | "DROP"
	Comment   string `json:"comment,omitempty"`
}

// FirewallConfig is the persisted rule list. Serialized to
// firewall.json in the state dir. Zero value = no rules (chain is
// empty, all traffic falls through to INPUT).
//
// Rules are stored in the canonical order (ACCEPTs first, then DROPs,
// each group preserving operator-authored order). See
// SortRulesAcceptFirst.
type FirewallConfig struct {
	Rules []FirewallRule `json:"rules"`
}

// SortRulesAcceptFirst returns rules reordered so every ACCEPT comes
// before every DROP, preserving operator-authored order within each
// group (stable partition). This matches iptables first-match-wins
// semantics for the "allow a few, deny the rest" use case and
// prevents the footgun of a broad DROP row above a specific ACCEPT
// making the ACCEPT unreachable.
//
// Rules with any action other than DROP (i.e. empty or ACCEPT) are
// treated as ACCEPT for sort purposes; Validate() normalizes empty to
// ACCEPT before this ever runs in the save path.
func SortRulesAcceptFirst(rules []FirewallRule) []FirewallRule {
	out := make([]FirewallRule, 0, len(rules))
	for _, r := range rules {
		if r.Action != "DROP" {
			out = append(out, r)
		}
	}
	for _, r := range rules {
		if r.Action == "DROP" {
			out = append(out, r)
		}
	}
	return out
}

// Validate checks the rule is well-formed enough to hand to iptables.
// Callers turn a non-nil error into a 400.
func (r *FirewallRule) Validate() error {
	r.CIDR = strings.TrimSpace(r.CIDR)
	r.Interface = strings.TrimSpace(r.Interface)
	r.Transport = strings.ToLower(strings.TrimSpace(r.Transport))
	r.Port = strings.TrimSpace(r.Port)
	r.Action = strings.ToUpper(strings.TrimSpace(r.Action))
	r.Comment = strings.TrimSpace(r.Comment)

	if r.Action == "" {
		r.Action = "ACCEPT"
	}
	if r.Action != "ACCEPT" && r.Action != "DROP" {
		return fmt.Errorf("action: must be ACCEPT or DROP (got %q)", r.Action)
	}

	if r.CIDR == "" {
		return errors.New("cidr required")
	}
	if _, _, err := net.ParseCIDR(r.CIDR); err != nil {
		// Accept a bare IP as /32 for convenience.
		if ip := net.ParseIP(r.CIDR); ip != nil && ip.To4() != nil {
			r.CIDR = ip.String() + "/32"
		} else {
			return fmt.Errorf("cidr: %v", err)
		}
	} else {
		// Reject IPv6 CIDRs — we only manage the v4 chain.
		if ip, _, _ := net.ParseCIDR(r.CIDR); ip.To4() == nil {
			return fmt.Errorf("cidr: IPv6 not supported (%s)", r.CIDR)
		}
	}
	if r.Interface != "" && !safeInterface(r.Interface) {
		return fmt.Errorf("interface: invalid name %q", r.Interface)
	}
	switch r.Transport {
	case "", "tcp", "udp":
	default:
		return fmt.Errorf("transport: must be tcp, udp, or empty (got %q)", r.Transport)
	}
	if r.Port != "" {
		if r.Transport == "" {
			return errors.New("port requires transport (tcp or udp)")
		}
		if err := validatePortSpec(r.Port); err != nil {
			return fmt.Errorf("port: %w", err)
		}
	}
	// Comment must not contain quotes or newlines — iptables comment
	// module is fragile and we build the restore payload as a string.
	for _, ch := range r.Comment {
		if ch == '"' || ch == '\n' || ch == '\r' || ch < 0x20 {
			return errors.New("comment: control chars and quotes not allowed")
		}
	}
	if len(r.Comment) > 128 {
		return errors.New("comment: max 128 chars")
	}
	return nil
}

func safeInterface(s string) bool {
	if len(s) == 0 || len(s) > 15 { // IFNAMSIZ - 1
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == ':'
		if !ok {
			return false
		}
	}
	return true
}

func validatePortSpec(s string) error {
	if strings.Contains(s, "-") {
		lo, hi, ok := strings.Cut(s, "-")
		if !ok {
			return fmt.Errorf("bad range %q", s)
		}
		lop, err := parsePort(lo)
		if err != nil {
			return err
		}
		hip, err := parsePort(hi)
		if err != nil {
			return err
		}
		if lop > hip {
			return fmt.Errorf("range start %d > end %d", lop, hip)
		}
		return nil
	}
	_, err := parsePort(s)
	return err
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("not a number: %q", s)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("%d out of range (1-65535)", n)
	}
	return n, nil
}

// firewallConfigPath returns the on-disk location of firewall.json for
// the given state dir. Callers pass the state dir explicitly so the
// model package doesn't reach into config.
func firewallConfigPath(stateDir string) string {
	return filepath.Join(stateDir, "firewall.json")
}

// LoadFirewallConfig reads firewall.json under stateDir. Missing file
// returns a zero FirewallConfig + nil — the "no rules yet" case.
// Rules without an explicit action default to ACCEPT on Validate(), so
// files written by the pre-Action version of ACE load unchanged.
//
// Rules are re-sorted through SortRulesAcceptFirst on load so a
// hand-edited firewall.json (or one written by an older ACE that did
// no sort) still applies in canonical order. Startup's ApplyPersisted
// therefore always emits ACCEPTs before DROPs.
func LoadFirewallConfig(stateDir string) (FirewallConfig, error) {
	p := firewallConfigPath(stateDir)
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FirewallConfig{}, nil
		}
		return FirewallConfig{}, err
	}
	defer f.Close()
	var out FirewallConfig
	if err := json.NewDecoder(f).Decode(&out); err != nil {
		return FirewallConfig{}, fmt.Errorf("parse %s: %w", p, err)
	}
	out.Rules = SortRulesAcceptFirst(out.Rules)
	return out, nil
}

// SaveFirewallConfig writes firewall.json under stateDir atomically.
// Creates stateDir if missing.
func SaveFirewallConfig(stateDir string, cfg FirewallConfig) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	p := firewallConfigPath(stateDir)
	tmp, err := os.CreateTemp(stateDir, ".firewall.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}
