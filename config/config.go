// Package config holds runtime configuration for the ace controller.
//
// Configuration is set at startup via flags; there is no runtime reload.
// Empty fields default to the zero-value behavior documented per field.
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the resolved runtime configuration.
type Config struct {
	// Addr is the HTTP bind address for the admin UI.
	Addr string

	// VoipPatrolBin is the absolute path to the voip_patrol binary the
	// controller will invoke. Required.
	VoipPatrolBin string

	// VoipPatrolPort is the local SIP port voip_patrol binds for each
	// run. The same port is reused across runs since runs are serialized
	// (one at a time) in v1.
	VoipPatrolPort int

	// PublicAddress is the public-side IP voip_patrol advertises in
	// Contact / Via, set via --public-address. Required when ace runs
	// behind NAT and the SBC needs to send BYE/re-INVITE back.
	PublicAddress string

	// ScenariosDir holds the XML scenario files the controller can run.
	ScenariosDir string

	// RunsDir is where per-run output lands. The controller creates a
	// timestamped subdir per run; results.json + captured WAVs go there.
	RunsDir string

	// BasicAuthHtpasswd, when non-empty, points at an htpasswd file
	// (bcrypt entries only) enforced by the handler middleware. Empty
	// disables auth entirely — matches the existing behavior for LAN
	// deployments. On authenticated requests the middleware stamps the
	// matched username onto the request context so downstream code
	// (Runner.Start) can record who launched a run.
	BasicAuthHtpasswd string
}

// FromFlags parses flags and validates the result. Exits with a clear
// error on any missing required field.
func FromFlags() *Config {
	c := &Config{}
	flag.StringVar(&c.Addr, "addr", "0.0.0.0:8086", "HTTP bind address")
	flag.StringVar(&c.VoipPatrolBin, "voip-patrol-bin", "/usr/local/bin/voip_patrol", "path to voip_patrol binary")
	flag.IntVar(&c.VoipPatrolPort, "voip-patrol-port", 5093, "local SIP port voip_patrol binds")
	flag.StringVar(&c.PublicAddress, "public-address", "", "public-side IP for SIP Contact / Via (passed to voip_patrol --public-address)")
	flag.StringVar(&c.ScenariosDir, "scenarios-dir", "./scenarios", "directory holding scenario XML files")
	flag.StringVar(&c.RunsDir, "runs-dir", "./runs", "directory where per-run output lands")
	flag.StringVar(&c.BasicAuthHtpasswd, "basic-auth-htpasswd", "", "path to htpasswd file (bcrypt); empty = no auth")
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

	return c
}
