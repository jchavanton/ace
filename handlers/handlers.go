// Package handlers wires the gin routes for the ace admin UI.
package handlers

import (
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
	r.POST("/scenarios", s.handleScenarioCreate)
	r.GET("/scenarios/:name", s.handleScenarioDetail)
	r.POST("/scenarios/:name", s.handleScenarioSave)
	r.POST("/scenarios/:name/run", s.handleRun)
	r.GET("/runs", s.handleRuns)
	r.GET("/runs/:id", s.handleRunDetail)
	r.GET("/runs/:id/log", s.handleRunLog)
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
	// Start launches voip_patrol in a background goroutine and returns
	// immediately with the run record (status=running). We redirect to
	// the run detail page; the goroutine outlives this HTTP request, so
	// navigating away or closing the tab doesn't kill voip_patrol.
	run, err := s.Runner.Start(scn)
	if err != nil {
		c.String(http.StatusConflict, "run failed to start: %v", err)
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
	// Read the last 200 lines of voip_patrol's log so the detail page
	// can show what's happening live. ReadFile error is ignored — empty
	// log is the running-but-no-output-yet state.
	logBytes, _ := os.ReadFile(filepath.Join(run.Dir(s.Cfg.RunsDir), "stdout.log"))
	c.HTML(http.StatusOK, "layout", gin.H{
		"Title":           "Run " + run.ID,
		"Page":            "runs",
		"ContentTemplate": "content_run_detail",
		"Run":             run,
		"WAVs":            run.WAVFiles(s.Cfg.RunsDir),
		"LogTail":         tailLines(string(logBytes), 200),
		"LogBytes":        len(logBytes),
	})
}

// handleRunLog serves the run's stdout.log as plain text. Optional
// `?tail=N` returns the last N lines (default: full file). Useful both
// directly (operators tailing via curl) and as a fallback when the
// detail page's inline log preview gets truncated.
func (s *Server) handleRunLog(c *gin.Context) {
	id := c.Param("id")
	run, err := models.LoadRun(s.Cfg.RunsDir, id)
	if err != nil {
		c.String(http.StatusNotFound, "run %q: %v", id, err)
		return
	}
	body, _ := os.ReadFile(filepath.Join(run.Dir(s.Cfg.RunsDir), "stdout.log"))
	if tail := c.Query("tail"); tail != "" {
		if n, err := strconv.Atoi(tail); err == nil && n > 0 {
			body = []byte(tailLines(string(body), n))
		}
	}
	c.Data(http.StatusOK, "text/plain; charset=utf-8", body)
}

// tailLines returns the last n lines of s. Empty result when s is empty.
// For huge logs we'd want to read from the end of the file; voip_patrol
// logs are bounded (few MB at most for a 60s run), so a whole-string
// pass is fine.
func tailLines(s string, n int) string {
	if s == "" || n <= 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n") + "\n"
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

// handleScenarioSave overwrites an existing scenario's XML on disk.
// Validates the submitted body parses as XML before writing; rejects
// otherwise so we don't replace a working scenario with garbage.
func (s *Server) handleScenarioSave(c *gin.Context) {
	name := sanitizeScenarioName(c.Param("name"))
	if name == "" {
		c.String(http.StatusBadRequest, "invalid name")
		return
	}
	xml := c.PostForm("xml")
	if strings.TrimSpace(xml) == "" {
		c.String(http.StatusBadRequest, "empty XML")
		return
	}
	if err := validateXML(xml); err != nil {
		c.String(http.StatusBadRequest, "XML parse error: %v", err)
		return
	}
	// Only allow saving over existing scenarios via this route. Creation
	// goes through POST /scenarios.
	path := filepath.Join(s.Cfg.ScenariosDir, name+".xml")
	if _, err := os.Stat(path); err != nil {
		c.String(http.StatusNotFound, "scenario %q does not exist; create it first", name)
		return
	}
	if err := os.WriteFile(path, []byte(xml), 0o644); err != nil {
		c.String(http.StatusInternalServerError, "write: %v", err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/scenarios/"+name)
}

// handleScenarioCreate creates a new scenario file. Requires a name
// (used as the .xml basename) and the XML body. Refuses to overwrite
// an existing file — use the save route for that.
func (s *Server) handleScenarioCreate(c *gin.Context) {
	name := sanitizeScenarioName(c.PostForm("name"))
	if name == "" {
		c.String(http.StatusBadRequest, "name required (letters, digits, _ and - only)")
		return
	}
	xml := c.PostForm("xml")
	if strings.TrimSpace(xml) == "" {
		// Provide a useful starter so the editor isn't empty.
		xml = scenarioTemplate
	}
	if err := validateXML(xml); err != nil {
		c.String(http.StatusBadRequest, "XML parse error: %v", err)
		return
	}
	path := filepath.Join(s.Cfg.ScenariosDir, name+".xml")
	if _, err := os.Stat(path); err == nil {
		c.String(http.StatusConflict, "scenario %q already exists", name)
		return
	}
	if err := os.WriteFile(path, []byte(xml), 0o644); err != nil {
		c.String(http.StatusInternalServerError, "write: %v", err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/scenarios/"+name)
}

// sanitizeScenarioName accepts letters, digits, '_', and '-' only. Any
// other character (slashes, dots, spaces, ...) → empty result → 400 at
// the handler. This is the only path validation between the URL/form
// param and the filesystem, so it must be strict.
func sanitizeScenarioName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return ""
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-'
		if !ok {
			return ""
		}
	}
	return s
}

// validateXML checks the body parses as a well-formed XML document.
// We don't enforce a voip_patrol schema beyond "parses" — that's what
// running the scenario will surface.
func validateXML(body string) error {
	dec := xml.NewDecoder(strings.NewReader(body))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// scenarioTemplate is what we drop into the editor when an operator
// creates a new scenario with no body. Minimal voip_patrol skeleton
// matching the shape of the existing aizan_test_option4 scenario.
const scenarioTemplate = `<config>
  <actions>
    <action type="codec" disable="all"/>
    <action type="codec" enable="pcmu" priority="250"/>
    <action type="codec" enable="pcma" priority="249"/>

    <action type="call" label="my-scenario"
            transport="udp"
            account="5145550199"
            expected_cause_code="200"
            caller="+15145550199@sbc.example.com"
            callee="+16477988128@sbc.example.com"
            max_duration="60" hangup="55"
            rtp_stats="true"
            record="true"
            play_dtmf="WW1#"/>

    <action type="wait" complete="true"/>
  </actions>
</config>
`
