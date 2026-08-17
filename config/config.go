// Package config holds runtime configuration for the ace controller.
//
// Configuration is set at startup via flags; there is no runtime reload.
// Empty fields default to the zero-value behavior documented per field.
package config

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Config is the resolved runtime configuration.
type Config struct {
	// Addr is the HTTP bind address for the admin UI.
	Addr string

	// VoipPatrolBin is the absolute path to the voip_patrol binary the
	// controller will invoke. Required.
	VoipPatrolBin string

	// VoipPatrolPort is the default local SIP port voip_patrol binds for
	// each run. Overridable per-run via the Run form. The same port is
	// reused across runs since runs are serialized (one at a time) in v1.
	VoipPatrolPort int

	// RTPPortStart / RTPPortEnd are the default RTP port range passed to
	// voip_patrol as --rtp-port / --rtp-port-end. Overridable per-run.
	// voip_patrol allocates the actual pair(s) from within this range.
	RTPPortStart int
	RTPPortEnd   int

	// DefaultTimeoutSeconds is the wall-clock cap on any single run
	// when the scenario doesn't specify its own. 0 = unlimited (no
	// controller-side deadline; voip_patrol runs until its scenario
	// completes or crashes). Scenarios can override per-file.
	DefaultTimeoutSeconds int

	// PublicAddress is the public-side IP voip_patrol advertises in
	// Contact / Via, set via --public-address. Required when ace runs
	// behind NAT and the SBC needs to send BYE/re-INVITE back.
	PublicAddress string

	// ScenariosDir holds the XML scenario files the controller can run.
	ScenariosDir string

	// RunsDir is where per-run output lands. The controller creates a
	// timestamped subdir per run; results.json + captured WAVs go there.
	RunsDir string

	// VoiceRefDir is the source dir for voip_patrol's reference WAV
	// files. Scenarios reference these by relative path (e.g.
	// "voice_ref_files/reference_8000.wav"), which voip_patrol
	// resolves against its cwd. We set cwd to the per-run dir, so the
	// runner symlinks this dir into each run dir as "voice_ref_files"
	// before spawning. Empty disables the symlink (scenarios that
	// don't use ref files still work).
	VoiceRefDir string

	// BasicAuthHtpasswd, when non-empty, points at an htpasswd file
	// (bcrypt entries only) enforced by the handler middleware. Empty
	// disables auth entirely — matches the existing behavior for LAN
	// deployments. On authenticated requests the middleware stamps the
	// matched username onto the request context so downstream code
	// (Runner.Start) can record who launched a run.
	BasicAuthHtpasswd string

	// LocalIPs holds the host's non-loopback IPv4 addresses, enumerated
	// once at startup. Shown in the scenario-detail UI as selectable
	// options for the per-scenario voip_patrol --ip-addr override.
	LocalIPs []string

	// DetectedPublicIP is the host's public IPv4 as reported by an
	// external service (ifconfig.me), fetched once at startup. Empty on
	// lookup failure — the UI just hides the "public" option in that
	// case. This is a UI convenience; the actual --ip-addr sent to
	// voip_patrol is whatever the scenario has selected (or the global
	// PublicAddress default).
	DetectedPublicIP string

	// FirewallStateDir is where the /firewall page persists its
	// firewall.json rule set. Empty disables the Firewall page entirely
	// (dev environments without iptables / NET_ADMIN just leave it off).
	FirewallStateDir string

	// FirewallSavePath, when non-empty, is where the runtime iptables
	// state is dumped after each apply so a reboot (or iptables-persistent)
	// can restore it. Typically "/etc/iptables/rules.v4".
	FirewallSavePath string

	// TLSCert / TLSPrivKey / TLSCAList are absolute paths inside the
	// container. When TLSCert and TLSPrivKey are both set, the runner
	// appends --tls-cert / --tls-privkey to every voip_patrol invocation
	// so scenarios that use transport="tls" can present a cert. TLSCAList
	// is optional (only needed when the peer's issuer isn't in pjsip's
	// default trust store, or when --tls-verify-server is used). Empty =
	// no TLS flags passed; a scenario using TLS will fail at handshake.
	TLSCert    string
	TLSPrivKey string
	TLSCAList  string
}

// FromFlags parses flags and validates the result. Exits with a clear
// error on any missing required field.
func FromFlags() *Config {
	c := &Config{}
	flag.StringVar(&c.Addr, "addr", "0.0.0.0:8086", "HTTP bind address")
	flag.StringVar(&c.VoipPatrolBin, "voip-patrol-bin", "/usr/local/bin/voip_patrol", "path to voip_patrol binary")
	flag.IntVar(&c.VoipPatrolPort, "voip-patrol-port", 5093, "default local SIP port voip_patrol binds (per-run override in UI)")
	flag.IntVar(&c.RTPPortStart, "rtp-port-start", 4000, "default RTP port range start (per-run override in UI)")
	flag.IntVar(&c.RTPPortEnd, "rtp-port-end", 14000, "default RTP port range end (per-run override in UI)")
	flag.IntVar(&c.DefaultTimeoutSeconds, "default-timeout-seconds", 0, "default wall-clock cap per run in seconds when the scenario doesn't set one; 0 = unlimited")
	flag.StringVar(&c.PublicAddress, "public-address", "", "public-side IP for SIP Contact / Via (passed to voip_patrol --ip-addr as the global default)")
	var localIPsRaw string
	flag.StringVar(&localIPsRaw, "local-ips", "", "comma-separated list of private IPs to surface in the UI Network dropdown; overrides in-container interface enumeration (needed on cloud VMs where the container's veth isn't the address to advertise)")
	flag.StringVar(&c.ScenariosDir, "scenarios-dir", "./scenarios", "directory holding scenario XML files")
	flag.StringVar(&c.RunsDir, "runs-dir", "./runs", "directory where per-run output lands")
	flag.StringVar(&c.VoiceRefDir, "voice-ref-dir", "/voice_ref_files", "source dir for voip_patrol reference WAVs; symlinked into each run dir as 'voice_ref_files'. Empty disables.")
	flag.StringVar(&c.BasicAuthHtpasswd, "basic-auth-htpasswd", "", "path to htpasswd file (bcrypt); empty = no auth")
	flag.StringVar(&c.FirewallStateDir, "firewall-state-dir", "", "directory holding firewall.json for the /firewall page; empty disables the page")
	flag.StringVar(&c.FirewallSavePath, "firewall-save-path", "", "iptables-save target for reboot-persistence (e.g. /etc/iptables/rules.v4); empty = runtime only")
	flag.StringVar(&c.TLSCert, "tls-cert", "", "TLS certificate file (pem) passed to voip_patrol --tls-cert; needs --tls-privkey to take effect")
	flag.StringVar(&c.TLSPrivKey, "tls-privkey", "", "TLS private key file (pem) passed to voip_patrol --tls-privkey")
	flag.StringVar(&c.TLSCAList, "tls-calist", "", "TLS CA list (pem) passed to voip_patrol --tls-calist; optional")
	flag.Parse()

	// Resolve to absolute paths so the gin handlers don't need to care
	// about the controller's cwd at request time.
	for _, p := range []*string{&c.ScenariosDir, &c.RunsDir} {
		abs, err := filepath.Abs(*p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ace: bad path %q: %v\n", *p, err)
			os.Exit(2)
		}
		*p = abs
	}

	// Ensure the dirs exist; this is a fresh-install convenience —
	// later runs will hit them whether or not we created them today.
	for _, p := range []string{c.ScenariosDir, c.RunsDir} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "ace: mkdir %s: %v\n", p, err)
			os.Exit(2)
		}
	}

	// -local-ips wins over interface enumeration — on cloud VMs the
	// container's veth is 172.x, but the operator wants to advertise the
	// VM's private address (10.x on GCP). Ansible sets ACE_LOCAL_IPS
	// from the metadata service; docker translates that to -local-ips.
	if localIPsRaw = strings.TrimSpace(localIPsRaw); localIPsRaw != "" {
		for _, s := range strings.Split(localIPsRaw, ",") {
			s = strings.TrimSpace(s)
			if s != "" && net.ParseIP(s) != nil {
				c.LocalIPs = append(c.LocalIPs, s)
			}
		}
	} else {
		c.LocalIPs = detectLocalIPs()
	}
	c.DetectedPublicIP = detectPublicIP()
	// PublicAddress from -public-address wins over the ifconfig.me
	// probe as the "detected public" the UI shows. Ansible pins it
	// from GCP metadata on cloud deploys; the probe stays as a
	// last-resort fallback for hand-launched local stacks.
	if c.DetectedPublicIP == "" && c.PublicAddress != "" && net.ParseIP(c.PublicAddress) != nil {
		c.DetectedPublicIP = c.PublicAddress
	}

	return c
}

// detectLocalIPs returns non-loopback IPv4 addresses assigned to the
// host's interfaces, sorted for stable UI ordering. Errors from
// net.Interfaces / interface.Addrs are swallowed — a machine with no
// enumerable interfaces just gets an empty list and the UI shows only
// the public option (or nothing).
func detectLocalIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip4 := ip.To4()
			if ip4 == nil {
				continue
			}
			s := ip4.String()
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// detectPublicIP fetches the host's public IPv4 from ifconfig.me. A
// short timeout keeps startup snappy on hosts with no outbound
// connectivity; any error just returns "". Called once at startup —
// this is a UI hint, not authoritative, so a stale value between
// restarts is acceptable.
func detectPublicIP() string {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest("GET", "https://ifconfig.me/ip", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "curl/8")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(body))
	if net.ParseIP(s) == nil {
		return ""
	}
	return s
}
