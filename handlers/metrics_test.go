// Copyright (C) 2026 Julien Chavanton <jchavanton@gmail.com>
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"strings"
	"testing"
)

// botPanels is the subset that must be filtered by started_by. The
// host_* panels are global (per-prometheus) and intentionally carry
// no %s. Keep this list in sync with promPanels — a bot-scoped panel
// missing from here would silently leak one bot's series into
// another's report.
var botPanels = map[string]bool{
	"runs_rate":  true,
	"calls_rate": true,
	"invite_p95": true,
	"mos_rx":     true,
	"rtt_avg":    true,
}

// TestPromPanelsHaveFilter guards each panel against the wrong filter
// shape: bot panels must contain exactly one `started_by=%s` matcher;
// host panels must contain no %s at all (an accidental %s on a host
// panel would be injected with a random started_by string from the
// querystring, bypassing the allowlist).
func TestPromPanelsHaveFilter(t *testing.T) {
	for name, tmpl := range promPanels {
		if botPanels[name] {
			if !strings.Contains(tmpl, "started_by=%s") {
				t.Errorf("bot panel %q template missing started_by=%%s filter: %s", name, tmpl)
			}
			if strings.Count(tmpl, "%s") != strings.Count(tmpl, "started_by=%s") {
				t.Errorf("bot panel %q has a %%s outside a started_by matcher (accidental interpolation surface): %s", name, tmpl)
			}
			continue
		}
		if strings.Contains(tmpl, "%s") {
			t.Errorf("host panel %q unexpectedly contains %%s — host panels must be global, no started_by: %s", name, tmpl)
		}
	}
}

// TestPromPanelsBalancedBraces catches a copy-paste that left an
// unclosed sum-by/histogram_quantile, which prometheus rejects as
// invalid PromQL. Cheap smoke test — the real PromQL syntax is only
// validated when we hit prometheus, but this fails locally.
func TestPromPanelsBalancedBraces(t *testing.T) {
	for name, tmpl := range promPanels {
		if strings.Count(tmpl, "(") != strings.Count(tmpl, ")") {
			t.Errorf("panel %q unbalanced parens: %s", name, tmpl)
		}
		if strings.Count(tmpl, "{") != strings.Count(tmpl, "}") {
			t.Errorf("panel %q unbalanced braces: %s", name, tmpl)
		}
	}
}
