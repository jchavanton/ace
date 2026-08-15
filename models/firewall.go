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

// FirewallRule is one ACCEPT entry the operator has added via the UI.
// The rule set is an allow-list — matches ACCEPT, misses fall through
// (chain default is RETURN, so anything else in INPUT still runs).
//
// Fields map 1:1 to iptables flags:
//
//	CIDR       -> -s <cidr>            (source; required)
//	Interface  -> -i <iface>           (input interface; optional, "" = any)
//	Transport  -> -p tcp|udp           (optional, "" = any protocol)
//	Port       -> --dport N or N:M     (optional; requires Transport when set)
//	Comment    -> -m comment --comment (optional; sanitized)
type FirewallRule struct {
	CIDR      string `json:"cidr"`
	Interface string `json:"interface,omitempty"`
	Transport string `json:"transport,omitempty"` // "" | "tcp" | "udp"
	Port      string `json:"port,omitempty"`      // "5093" or "4000-14000"
	Comment   string `json:"comment,omitempty"`
}

// FirewallConfig is the persisted allow-list plus the optional
// protected port range that gets DROP'd at the end of the chain.
// Serialized to firewall.json in the state dir.
//
// ProtectedPorts is a port spec ("5060-5090" or "5060") that ACE
// appends as a terminal DROP for both tcp and udp after the allow-list.
// Empty string = no drop (chain falls through to the rest of INPUT).
type FirewallConfig struct {
	Rules          []FirewallRule `json:"rules"`
	ProtectedPorts string         `json:"protected_ports,omitempty"`
}

// DefaultProtectedPorts is what a fresh install gets when firewall.json
// doesn't exist yet — the SIP signaling window used by voip_patrol and
// aizan scenarios. Kept out of the zero value so callers can distinguish
// "operator explicitly cleared it" from "never configured."
const DefaultProtectedPorts = "5060-5090"

// ValidateProtectedPorts returns nil for an empty spec (drop disabled)
// or a well-formed port / port range. Handlers turn a non-nil error
// into a 400.
func ValidateProtectedPorts(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return validatePortSpec(s)
}

// Validate checks the rule is well-formed enough to hand to iptables.
// Callers turn a non-nil error into a 400.
func (r *FirewallRule) Validate() error {
	r.CIDR = strings.TrimSpace(r.CIDR)
	r.Interface = strings.TrimSpace(r.Interface)
	r.Transport = strings.ToLower(strings.TrimSpace(r.Transport))
	r.Port = strings.TrimSpace(r.Port)
	r.Comment = strings.TrimSpace(r.Comment)

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
// returns an empty rule set with the default protected port range — a
// fresh install ships with SIP DROP'd until the operator adds
// allow-list entries.
func LoadFirewallConfig(stateDir string) (FirewallConfig, error) {
	p := firewallConfigPath(stateDir)
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FirewallConfig{ProtectedPorts: DefaultProtectedPorts}, nil
		}
		return FirewallConfig{}, err
	}
	defer f.Close()
	var out FirewallConfig
	if err := json.NewDecoder(f).Decode(&out); err != nil {
		return FirewallConfig{}, fmt.Errorf("parse %s: %w", p, err)
	}
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
