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
	srv.Register(r)

	log.Printf("ace listening on http://%s (voip_patrol=%s scenarios=%s runs=%s)",
		cfg.Addr, cfg.VoipPatrolBin, cfg.ScenariosDir, cfg.RunsDir)
	if err := http.ListenAndServe(cfg.Addr, r); err != nil {
		log.Fatal(err)
	}
}
