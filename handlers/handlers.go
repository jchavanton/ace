// Package handlers wires the gin routes for the ace admin UI.
package handlers

import (
	"context"
	"net/http"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jchavanton/ace/config"
	"github.com/jchavanton/ace/controller"
	"github.com/jchavanton/ace/models"
)

// Server holds shared state for the handlers.
type Server struct {
	Cfg    *config.Config
	Runner *controller.Runner
}

// Register installs the routes on r.
func (s *Server) Register(r *gin.Engine) {
	r.GET("/", s.handleIndex)
	r.GET("/scenarios", s.handleScenarios)
	r.GET("/scenarios/:name", s.handleScenarioDetail)
	r.POST("/scenarios/:name/run", s.handleRun)
	r.GET("/runs", s.handleRuns)
	r.GET("/runs/:id", s.handleRunDetail)
	r.GET("/runs/:id/wav/:file", s.handleRunWAV)
}

func (s *Server) handleIndex(c *gin.Context) {
	c.Redirect(http.StatusSeeOther, "/scenarios")
}

func (s *Server) handleScenarios(c *gin.Context) {
	scenarios, err := models.LoadScenarios(s.Cfg.ScenariosDir)
	if err != nil {
		c.String(http.StatusInternalServerError, "load scenarios: %v", err)
		return
	}
	runs, _ := models.ListRuns(s.Cfg.RunsDir)
	if len(runs) > 5 {
		runs = runs[:5]
	}
	c.HTML(http.StatusOK, "layout", gin.H{
		"Title":           "Scenarios",
		"Page":            "scenarios",
		"ContentTemplate": "content_scenarios",
		"Scenarios":       scenarios,
		"RecentRuns":      runs,
		"Busy":            s.Runner.IsBusy(),
		"ScenariosDir":    s.Cfg.ScenariosDir,
	})
}

func (s *Server) handleScenarioDetail(c *gin.Context) {
	name := c.Param("name")
	scn, err := models.LoadScenario(s.Cfg.ScenariosDir, name)
	if err != nil {
		c.String(http.StatusNotFound, "scenario %q: %v", name, err)
		return
	}
	xml, err := scn.ReadXML()
	if err != nil {
		c.String(http.StatusInternalServerError, "read xml: %v", err)
		return
	}
	c.HTML(http.StatusOK, "layout", gin.H{
		"Title":           scn.Name,
		"Page":            "scenarios",
		"ContentTemplate": "content_scenario_detail",
		"Scenario":        scn,
		"XML":             xml,
		"Busy":            s.Runner.IsBusy(),
	})
}

func (s *Server) handleRun(c *gin.Context) {
	name := c.Param("name")
	scn, err := models.LoadScenario(s.Cfg.ScenariosDir, name)
	if err != nil {
		c.String(http.StatusNotFound, "scenario %q: %v", name, err)
		return
	}
	if s.Runner.IsBusy() {
		c.String(http.StatusConflict, "another run is in progress; one at a time")
		return
	}
	// Time-bound the run so a stuck voip_patrol doesn't block the
	// controller forever. 10 min is well above any reasonable scenario.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Minute)
	defer cancel()
	run, err := s.Runner.Run(ctx, scn)
	if err != nil && run == nil {
		c.String(http.StatusInternalServerError, "run failed: %v", err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/runs/"+run.ID)
}

func (s *Server) handleRuns(c *gin.Context) {
	runs, err := models.ListRuns(s.Cfg.RunsDir)
	if err != nil {
		c.String(http.StatusInternalServerError, "list runs: %v", err)
		return
	}
	c.HTML(http.StatusOK, "layout", gin.H{
		"Title":           "Runs",
		"Page":            "runs",
		"ContentTemplate": "content_runs",
		"Runs":            runs,
	})
}

func (s *Server) handleRunDetail(c *gin.Context) {
	id := c.Param("id")
	run, err := models.LoadRun(s.Cfg.RunsDir, id)
	if err != nil {
		c.String(http.StatusNotFound, "run %q: %v", id, err)
		return
	}
	c.HTML(http.StatusOK, "layout", gin.H{
		"Title":           "Run " + run.ID,
		"Page":            "runs",
		"ContentTemplate": "content_run_detail",
		"Run":             run,
		"WAVs":            run.WAVFiles(s.Cfg.RunsDir),
	})
}

// handleRunWAV serves recorded WAVs out of the run's dir. Path is
// constrained to a single filename (no slashes via filepath.Base) so a
// crafted ":file" param can't escape into the parent dir.
func (s *Server) handleRunWAV(c *gin.Context) {
	id := c.Param("id")
	file := filepath.Base(c.Param("file"))
	run, err := models.LoadRun(s.Cfg.RunsDir, id)
	if err != nil {
		c.String(http.StatusNotFound, "run %q: %v", id, err)
		return
	}
	c.File(filepath.Join(run.Dir(s.Cfg.RunsDir), file))
}
