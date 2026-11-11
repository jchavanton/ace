// Copyright (C) 2026 Julien Chavanton <jchavanton@gmail.com>
// SPDX-License-Identifier: Apache-2.0

// Command ace is the voip_patrol controller: a small admin UI that
// lists scenario XML files, runs them via the voip_patrol binary,
// parses results, and shows per-call pass/fail + RTP/SIP stats.
//
// Usage:
//
//	ace -addr 0.0.0.0:8086 \
//	    -voip-patrol-bin /usr/local/bin/voip_patrol \
//	    -public-address 24.122.254.8 \
//	    -scenarios-dir ./scenarios \
//	    -runs-dir ./runs
//
// See SPEC.md for the design.
package main

import (
	"flag"
	"html/template"
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/jchavanton/ace/config"
	"github.com/jchavanton/ace/controller"
	"github.com/jchavanton/ace/handlers"
)

func main() {
	// Backfill flags: --reaggregate recomputes every run's Aggregate and
	// prints a diff; --apply also writes changed run.json files (with a
	// run.json.bak-<yyyymmdd> sidecar first). One-shot: exits before the
	// HTTP server starts.
	reaggregate := flag.Bool("reaggregate", false, "recompute Aggregate for every run on disk and exit; dry-run unless --apply")
	reaggregateApply := flag.Bool("apply", false, "with --reaggregate: actually write changed run.json files (default dry-run)")

	cfg := config.FromFlags()

	if *reaggregate {
		runner := &controller.Runner{Cfg: cfg}
		results, err := runner.ReaggregateRuns(*reaggregateApply)
		if err != nil {
			log.Fatalf("ace: reaggregate: %v", err)
		}
		mode := "dry-run"
		if *reaggregateApply {
			mode = "APPLY"
		}
		changed, errored := 0, 0
		for _, r := range results {
			if r.Error != nil {
				errored++
				log.Printf("[%s] %s: error: %v", mode, r.ID, r.Error)
				continue
			}
			if r.Skipped != "" {
				continue
			}
			if r.Changed {
				changed++
				log.Printf("[%s] %s: %s", mode, r.ID, r.Diff())
			}
		}
		log.Printf("[%s] %d runs, %d changed, %d errored", mode, len(results), changed, errored)
		if errored > 0 {
			os.Exit(1)
		}
		return
	}

	r := gin.Default()
	// `mul` lets templates convert voip_patrol's voice_frames (100 ms
	// sampler ticks) to a millisecond display without a parallel field
	// on CallResult. Keep this list tight; template arithmetic is a
	// smell, so each entry needs justification.
	r.SetFuncMap(template.FuncMap{
		"mul": func(a, b int) int { return a * b },
	})
	r.LoadHTMLGlob("templates/*.html")
	r.Static("/static", "./static")

	// /metrics is registered BEFORE srv.Register so it lives outside the
	// basic-auth middleware — prometheus scrapes without creds. The
	// scrape target is localhost from the sibling prometheus container
	// (host networking), so there's no cross-network exposure risk.
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	runner := &controller.Runner{Cfg: cfg}
	// Webhook poster is opt-in via -webhook-url. NewWebhookPoster returns
	// nil when the URL is empty; the hooks below are also nil-safe, so
	// skipping the registration isn't necessary — but we do it anyway to
	// keep the "nothing posted" case out of the per-call hot path.
	if poster := controller.NewWebhookPoster(cfg.WebhookURL, cfg.WebhookSecret, cfg.WebhookTimeout); poster != nil {
		runner.AddOnCall(poster.PostCall)
		runner.AddOnFinish(poster.PostRunFinished)
		log.Printf("ace: webhook enabled → %s", cfg.WebhookURL)
	}
	srv := &handlers.Server{
		Cfg:    cfg,
		Runner: runner,
	}
	sched := &controller.Scheduler{Cfg: cfg, Runner: runner}
	sched.Start()
	hostMon := &controller.HostMonitor{Cfg: cfg}
	hostMon.Start()
	cleanup := &controller.Cleanup{Cfg: cfg, Runner: runner}
	cleanup.Start()
	srv.Cleanup = cleanup
	// Any run left in status=running from a previous process is a lie
	// after this restart — mark them errored before we serve, so the UI
	// (and the Stop button in particular) doesn't act on ghost runs.
	if n, err := srv.Runner.RecoverOrphanedRuns(); err != nil {
		log.Printf("ace: recover orphaned runs: %v", err)
	} else if n > 0 {
		log.Printf("ace: marked %d orphaned run(s) as errored", n)
	}
	// Auto-runner has to come after orphan recovery so a previously
	// in-flight auto-run has been marked errored on disk; otherwise the
	// scenario detail page would show two overlapping "running" rows
	// until the stale one aged out of the UI.
	autoRun := &controller.AutoRunner{Cfg: cfg, Runner: runner}
	autoRun.Start()
	// Firewall management is opt-in. When enabled, load the persisted
	// rules and push them into iptables so a container restart doesn't
	// silently drop the operator's allow-list. Errors are logged but
	// don't fail startup — the /firewall page will show the same error
	// and the operator can fix it (missing NET_ADMIN, missing iptables
	// binary) without an ace restart.
	if cfg.FirewallStateDir != "" {
		fw := controller.NewFirewall(cfg.FirewallStateDir)
		fw.SavePath = cfg.FirewallSavePath
		srv.Firewall = fw
		if err := fw.ApplyPersisted(); err != nil {
			log.Printf("ace: firewall apply on startup: %v", err)
		}
	}
	srv.Register(r)

	log.Printf("ace listening on http://%s (voip_patrol=%s scenarios=%s runs=%s)",
		cfg.Addr, cfg.VoipPatrolBin, cfg.ScenariosDir, cfg.RunsDir)
	if err := http.ListenAndServe(cfg.Addr, r); err != nil {
		log.Fatal(err)
	}
}
