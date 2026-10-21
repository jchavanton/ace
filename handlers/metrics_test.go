// Copyright (C) 2026 Julien Chavanton <jchavanton@gmail.com>
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"strings"
	"testing"
)

// TestPromPanelsHaveFilter guards against a template forgetting the %s
// placeholder — a template without it would silently return unfiltered
// data for every bot, leaking one bot's series into another's report.
func TestPromPanelsHaveFilter(t *testing.T) {
	for name, tmpl := range promPanels {
		if !strings.Contains(tmpl, "started_by=%s") {
			t.Errorf("panel %q template missing started_by=%%s filter: %s", name, tmpl)
		}
		if strings.Count(tmpl, "%s") != strings.Count(tmpl, "started_by=%s") {
			t.Errorf("panel %q has a %%s outside a started_by matcher (accidental interpolation surface): %s", name, tmpl)
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
