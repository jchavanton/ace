// Package handlers wires the gin routes for the ace admin UI.
package handlers

import (
	"encoding/xml"
	"fmt"
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

// Register installs the routes on r. When Cfg.BasicAuthHtpasswd is set,
// a Basic-Auth middleware runs on every route; empty = no auth (matches
// the LAN-only default).
func (s *Server) Register(r *gin.Engine) {
	r.Use(basicAuth(s.Cfg.BasicAuthHtpasswd))
	r.GET("/", s.handleIndex)
	r.GET("/scenarios", s.handleScenarios)
	r.POST("/scenarios", s.handleScenarioCreate)
	r.GET("/scenarios/:name", s.handleScenarioDetail)
	r.POST("/scenarios/:name", s.handleScenarioSave)
	r.POST("/scenarios/:name/delete", s.handleScenarioDelete)
	r.POST("/scenarios/:name/ports", s.handleScenarioPortsSave)
	r.POST("/scenarios/:name/run", s.handleRun)
	r.POST("/runs/:id/stop", s.handleRunStop)
	r.GET("/runs", s.handleRuns)
	r.GET("/runs/:id", s.handleRunDetail)
	r.GET("/runs/:id/log", s.handleRunLog)
	r.GET("/runs/:id/wav/:file", s.handleRunWAV)
	r.POST("/runs/:id/delete", s.handleRunDelete)
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
		"ActiveRuns":      s.Runner.ActiveRuns(),
		// DefaultsBusy gates the list-view "Run" button, which submits
		// without per-run overrides. If the runner's default ports are
		// occupied, that button would just 409 — disable it up front.
		"DefaultsBusy": s.Runner.DefaultPortsInUse(),
		"ScenariosDir": s.Cfg.ScenariosDir,
		// Global defaults, shown greyed-out in the Ports column for
		// scenarios without saved overrides so users see what the
		// list-view "Run" button would actually use.
		"DefaultSIPPort":  s.Cfg.VoipPatrolPort,
		"DefaultRTPStart": s.Cfg.RTPPortStart,
		"DefaultRTPEnd":   s.Cfg.RTPPortEnd,
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
	// Prefill: scenario-saved ports override the runner defaults on a
	// per-field basis. That way a scenario can save only SIP, leaving
	// RTP to the runner's default, and the form reflects that mix.
	sipDefault := s.Cfg.VoipPatrolPort
	rtpStartDefault := s.Cfg.RTPPortStart
	rtpEndDefault := s.Cfg.RTPPortEnd
	if scn.Ports.SIP != 0 {
		sipDefault = scn.Ports.SIP
	}
	if scn.Ports.RTPPortStart != 0 {
		rtpStartDefault = scn.Ports.RTPPortStart
	}
	if scn.Ports.RTPPortEnd != 0 {
		rtpEndDefault = scn.Ports.RTPPortEnd
	}
	c.HTML(http.StatusOK, "layout", gin.H{
		"Title":           scn.Name,
		"Page":            "scenarios",
		"ContentTemplate": "content_scenario_detail",
		"Scenario":        scn,
		"XML":             xml,
		// ScenarioRunning gates the Delete button — same scenario file
		// being read by a live run shouldn't be removed. Run is now
		// always enabled since users can pick different ports.
		"ScenarioRunning": s.Runner.IsScenarioRunning(scn.Name),
		"ActiveRuns":      s.Runner.ActiveRuns(),
		"DefaultSIPPort":  sipDefault,
		"DefaultRTPStart": rtpStartDefault,
		"DefaultRTPEnd":   rtpEndDefault,
		// HasSavedPorts controls whether the "Reset saved ports" hint
		// shows next to the Save button.
		"HasSavedPorts": scn.Ports != (models.ScenarioPorts{}),
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
	// Prefer the authenticated user from the basic-auth middleware; fall
	// back to X-Forwarded-Email so the field stays populated if an
	// oauth2-proxy is later put in front. Empty when auth is disabled —
	// StartedBy just stays blank in run.json.
	user := c.GetString(ctxUserKey)
	if user == "" {
		user = c.GetHeader("X-Forwarded-Email")
	}
	// Port overrides from the Run form. Empty strings → 0. Precedence
	// is: form value → scenario's saved ports → runner config
	// defaults (the runner does the last step itself). We apply the
	// middle step here so a bare "Run" from the list submits the
	// scenario's saved ports without needing the detail page.
	ports, err := parsePorts(c)
	if err != nil {
		c.String(http.StatusBadRequest, "%v", err)
		return
	}
	if ports.SIP == 0 {
		ports.SIP = scn.Ports.SIP
	}
	if ports.RTPPortStart == 0 {
		ports.RTPPortStart = scn.Ports.RTPPortStart
	}
	if ports.RTPPortEnd == 0 {
		ports.RTPPortEnd = scn.Ports.RTPPortEnd
	}
	run, err := s.Runner.Start(scn, user, ports)
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

// handleScenarioPortsSave persists the per-scenario default SIP + RTP
// ports into a sidecar `<name>.ports.json` file next to the XML.
// Empty field(s) mean "clear that value" — writes a partial file.
// Fully empty submission removes the sidecar entirely (equivalent to
// "revert to runner defaults").
func (s *Server) handleScenarioPortsSave(c *gin.Context) {
	name := sanitizeScenarioName(c.Param("name"))
	if name == "" {
		c.String(http.StatusBadRequest, "invalid name")
		return
	}
	// Verify the scenario exists — no point saving ports for a
	// scenario that doesn't; also 404 vs. creating an orphan sidecar.
	if _, err := os.Stat(filepath.Join(s.Cfg.ScenariosDir, name+".xml")); err != nil {
		c.String(http.StatusNotFound, "scenario %q does not exist", name)
		return
	}
	ports, err := parsePorts(c)
	if err != nil {
		c.String(http.StatusBadRequest, "%v", err)
		return
	}
	sp := models.ScenarioPorts{
		SIP:          ports.SIP,
		RTPPortStart: ports.RTPPortStart,
		RTPPortEnd:   ports.RTPPortEnd,
	}
	if sp == (models.ScenarioPorts{}) {
		// All fields blank — treat as "revert to runner defaults" by
		// removing the sidecar. Nice symmetry with a fresh scenario.
		if err := models.DeleteScenarioPorts(s.Cfg.ScenariosDir, name); err != nil {
			c.String(http.StatusInternalServerError, "delete ports: %v", err)
			return
		}
	} else {
		if err := models.SaveScenarioPorts(s.Cfg.ScenariosDir, name, sp); err != nil {
			c.String(http.StatusInternalServerError, "save ports: %v", err)
			return
		}
	}
	c.Redirect(http.StatusSeeOther, "/scenarios/"+name)
}

// handleScenarioDelete removes a scenario's XML file. Refuses only if
// this specific scenario is currently being run — voip_patrol may
// still be reading the file. Other active runs against different
// scenarios don't block the delete.
func (s *Server) handleScenarioDelete(c *gin.Context) {
	name := sanitizeScenarioName(c.Param("name"))
	if name == "" {
		c.String(http.StatusBadRequest, "invalid name")
		return
	}
	if s.Runner.IsScenarioRunning(name) {
		c.String(http.StatusConflict, "scenario %q is currently running; stop the run first", name)
		return
	}
	path := filepath.Join(s.Cfg.ScenariosDir, name+".xml")
	if _, err := os.Stat(path); err != nil {
		c.String(http.StatusNotFound, "scenario %q does not exist", name)
		return
	}
	if err := os.Remove(path); err != nil {
		c.String(http.StatusInternalServerError, "delete: %v", err)
		return
	}
	// Sidecar is best-effort — if it's already gone or was never
	// there, the scenario delete still succeeds.
	_ = models.DeleteScenarioPorts(s.Cfg.ScenariosDir, name)
	c.Redirect(http.StatusSeeOther, "/scenarios")
}

// handleRunStop asks the runner to kill the currently-executing
// voip_patrol. Refuses if the id doesn't match the current run (a
// stale browser tab firing /runs/<old>/stop shouldn't kill an
// unrelated new run) or if nothing is running at all.
func (s *Server) handleRunStop(c *gin.Context) {
	id := sanitizeRunID(c.Param("id"))
	if id == "" {
		c.String(http.StatusBadRequest, "invalid run id")
		return
	}
	user := c.GetString(ctxUserKey)
	if user == "" {
		user = c.GetHeader("X-Forwarded-Email")
	}
	if user == "" {
		user = "anonymous"
	}
	if err := s.Runner.Stop(id, user); err != nil {
		c.String(http.StatusConflict, "%v", err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/runs/"+id)
}

// handleRunDelete removes a run's directory (run.json, stdout.log,
// results, WAVs). Refuses if the run is still marked "running" so we
// don't yank output from under the runner goroutine.
func (s *Server) handleRunDelete(c *gin.Context) {
	id := sanitizeRunID(c.Param("id"))
	if id == "" {
		c.String(http.StatusBadRequest, "invalid run id")
		return
	}
	run, err := models.LoadRun(s.Cfg.RunsDir, id)
	if err != nil {
		c.String(http.StatusNotFound, "run %q: %v", id, err)
		return
	}
	if run.Status == "running" {
		c.String(http.StatusConflict, "run %q is still running", id)
		return
	}
	if err := os.RemoveAll(run.Dir(s.Cfg.RunsDir)); err != nil {
		c.String(http.StatusInternalServerError, "delete: %v", err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/runs")
}

// parsePorts reads the optional sip_port / rtp_start / rtp_end fields
// from a Run form. Empty = 0 = "use config default" (runner does the
// fallback). Non-empty values must parse and be inside the
// unprivileged-port range; rtp_start must be <= rtp_end when both are
// set. Callers turn a non-nil error into a 400.
func parsePorts(c *gin.Context) (controller.Ports, error) {
	var p controller.Ports
	var err error
	if p.SIP, err = parsePortField(c.PostForm("sip_port"), "sip_port"); err != nil {
		return p, err
	}
	if p.RTPPortStart, err = parsePortField(c.PostForm("rtp_start"), "rtp_start"); err != nil {
		return p, err
	}
	if p.RTPPortEnd, err = parsePortField(c.PostForm("rtp_end"), "rtp_end"); err != nil {
		return p, err
	}
	if p.RTPPortStart != 0 && p.RTPPortEnd != 0 && p.RTPPortStart > p.RTPPortEnd {
		return p, fmt.Errorf("rtp_start (%d) must be <= rtp_end (%d)", p.RTPPortStart, p.RTPPortEnd)
	}
	return p, nil
}

func parsePortField(raw, name string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: not a number: %q", name, raw)
	}
	if n < 1024 || n > 65535 {
		return 0, fmt.Errorf("%s: %d out of range (1024-65535)", name, n)
	}
	return n, nil
}

// sanitizeRunID accepts the shape produced by models.NewRun:
// "YYYYMMDD-HHMMSS-<scenario>", i.e. letters, digits, '_', and '-'.
// Reject any '/' or '.' so a crafted id can't escape RunsDir into a
// sibling path (delete uses RemoveAll — path escape here is serious).
func sanitizeRunID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 128 {
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
