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
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jchavanton/ace/config"
	"github.com/jchavanton/ace/controller"
	"github.com/jchavanton/ace/handlers"
)

func main() {
	cfg := config.FromFlags()

	r := gin.Default()
	r.LoadHTMLGlob("templates/*.html")
	r.Static("/static", "./static")

	srv := &handlers.Server{
		Cfg:    cfg,
		Runner: &controller.Runner{Cfg: cfg},
	}
	// Any run left in status=running from a previous process is a lie
	// after this restart — mark them errored before we serve, so the UI
	// (and the Stop button in particular) doesn't act on ghost runs.
	if n, err := srv.Runner.RecoverOrphanedRuns(); err != nil {
		log.Printf("ace: recover orphaned runs: %v", err)
	} else if n > 0 {
		log.Printf("ace: marked %d orphaned run(s) as errored", n)
	}
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
