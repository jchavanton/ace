package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jchavanton/ace/controller"
	"github.com/jchavanton/ace/models"
)

// handleBots renders the /bots page: list of scheduled bots + recent
// alerts + the "new bot" form.
func (s *Server) handleBots(c *gin.Context) {
	bots, err := models.ListBots(s.Cfg.BotsDir)
	if err != nil {
		c.String(http.StatusInternalServerError, "list bots: %v", err)
		return
	}
	alerts, err := models.ListAlerts(s.Cfg.BotsDir)
	if err != nil {
		c.String(http.StatusInternalServerError, "list alerts: %v", err)
		return
	}
	// Cap the alert list — we render up to 100; older entries stay in
	// the file but aren't paginated in v1.
	if len(alerts) > 100 {
		alerts = alerts[:100]
	}
	scenarios, err := models.LoadScenarios(s.Cfg.ScenariosDir)
	if err != nil {
		c.String(http.StatusInternalServerError, "load scenarios: %v", err)
		return
	}
	unacked := models.UnackedAlertCount(s.Cfg.BotsDir)
	s.render(c, http.StatusOK, gin.H{
		"Title":           "Bots",
		"Page":            "bots",
		"ContentTemplate": "content_bots",
		"Bots":            bots,
		"Alerts":          alerts,
		"Scenarios":       scenarios,
		"UnackedAlerts":   unacked,
	})
}

// handleBotCreate creates a new bot from the /bots form. Requires a
// unique name, an existing scenario, and a positive interval.
func (s *Server) handleBotCreate(c *gin.Context) {
	name := sanitizeScenarioName(c.PostForm("name"))
	if name == "" {
		c.String(http.StatusBadRequest, "name required (letters, digits, _ and - only)")
		return
	}
	scenario := sanitizeScenarioName(c.PostForm("scenario"))
	if scenario == "" {
		c.String(http.StatusBadRequest, "scenario required")
		return
	}
	if _, err := models.LoadScenario(s.Cfg.ScenariosDir, scenario); err != nil {
		c.String(http.StatusBadRequest, "scenario %q does not exist", scenario)
		return
	}
	interval, err := strconv.Atoi(strings.TrimSpace(c.PostForm("interval_minutes")))
	if err != nil || interval < 1 {
		c.String(http.StatusBadRequest, "interval_minutes must be a positive integer")
		return
	}
	// Refuse to clobber an existing bot — use edit route to change it.
	if existing, _ := models.LoadBot(s.Cfg.BotsDir, name); existing != nil {
		c.String(http.StatusConflict, "bot %q already exists", name)
		return
	}
	enabled := c.PostForm("enabled") == "on"
	b := &models.Bot{
		Name:            name,
		Scenario:        scenario,
		IntervalMinutes: interval,
		Enabled:         enabled,
		// Leave NextRunAt zero so the scheduler treats a fresh enabled bot
		// as "fire on next tick" rather than making the operator wait one
		// full interval.
	}
	if err := models.SaveBot(s.Cfg.BotsDir, b); err != nil {
		c.String(http.StatusInternalServerError, "save: %v", err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/bots")
}

// handleBotUpdate edits an existing bot's schedule / enable flag /
// scenario. Name is immutable (it's the URL slug and file basename).
func (s *Server) handleBotUpdate(c *gin.Context) {
	name := sanitizeScenarioName(c.Param("name"))
	if name == "" {
		c.String(http.StatusBadRequest, "invalid name")
		return
	}
	b, err := models.LoadBot(s.Cfg.BotsDir, name)
	if err != nil {
		c.String(http.StatusNotFound, "bot %q: %v", name, err)
		return
	}
	if scn := sanitizeScenarioName(c.PostForm("scenario")); scn != "" {
		if _, err := models.LoadScenario(s.Cfg.ScenariosDir, scn); err != nil {
			c.String(http.StatusBadRequest, "scenario %q does not exist", scn)
			return
		}
		b.Scenario = scn
	}
	if raw := strings.TrimSpace(c.PostForm("interval_minutes")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			c.String(http.StatusBadRequest, "interval_minutes must be a positive integer")
			return
		}
		b.IntervalMinutes = n
	}
	b.Enabled = c.PostForm("enabled") == "on"
	if err := models.SaveBot(s.Cfg.BotsDir, b); err != nil {
		c.String(http.StatusInternalServerError, "save: %v", err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/bots")
}

// handleBotToggle flips a bot's Enabled field without touching the
// schedule. Used by the small enable/disable button in the list row.
func (s *Server) handleBotToggle(c *gin.Context) {
	name := sanitizeScenarioName(c.Param("name"))
	if name == "" {
		c.String(http.StatusBadRequest, "invalid name")
		return
	}
	b, err := models.LoadBot(s.Cfg.BotsDir, name)
	if err != nil {
		c.String(http.StatusNotFound, "bot %q: %v", name, err)
		return
	}
	b.Enabled = !b.Enabled
	// If re-enabling, reset NextRunAt so the scheduler fires on the next
	// tick rather than waiting for a stale (past-interval) timestamp
	// that may or may not still be in the future.
	if b.Enabled {
		b.NextRunAt = time.Time{}
	}
	if err := models.SaveBot(s.Cfg.BotsDir, b); err != nil {
		c.String(http.StatusInternalServerError, "save: %v", err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/bots")
}

// handleBotDelete removes a bot. Alerts filed against this bot stay in
// alerts.json — historical record, and the operator can ack them.
func (s *Server) handleBotDelete(c *gin.Context) {
	name := sanitizeScenarioName(c.Param("name"))
	if name == "" {
		c.String(http.StatusBadRequest, "invalid name")
		return
	}
	if err := models.DeleteBot(s.Cfg.BotsDir, name); err != nil {
		c.String(http.StatusInternalServerError, "delete: %v", err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/bots")
}

// handleBotRun fires the bot's scenario immediately (bypassing the
// schedule). Useful for testing a bot after creating it. NextRunAt is
// left untouched — this is a one-off, not a reschedule.
func (s *Server) handleBotRun(c *gin.Context) {
	name := sanitizeScenarioName(c.Param("name"))
	if name == "" {
		c.String(http.StatusBadRequest, "invalid name")
		return
	}
	b, err := models.LoadBot(s.Cfg.BotsDir, name)
	if err != nil {
		c.String(http.StatusNotFound, "bot %q: %v", name, err)
		return
	}
	scn, err := models.LoadScenario(s.Cfg.ScenariosDir, b.Scenario)
	if err != nil {
		c.String(http.StatusBadRequest, "scenario %q: %v", b.Scenario, err)
		return
	}
	ports := controller.Ports{
		SIP:            scn.Ports.SIP,
		RTPPortStart:   scn.Ports.RTPPortStart,
		RTPPortEnd:     scn.Ports.RTPPortEnd,
		PublicAddress:  scn.Ports.PublicAddress,
		TimeoutSeconds: scn.Ports.TimeoutSeconds,
		Transport:      scn.Ports.Transport,
		Nameservers:    scn.Ports.Nameservers,
	}
	user := c.GetString(ctxUserKey)
	if user == "" {
		user = "manual-bot-run"
	}
	run, err := s.Runner.StartWithTrigger(scn, user, "bot:"+b.Name, ports)
	if err != nil {
		c.String(http.StatusConflict, "start: %v", err)
		return
	}
	b.LastRunID = run.ID
	b.LastStatus = "running"
	b.LastRunAt = time.Now().UTC()
	_ = models.SaveBot(s.Cfg.BotsDir, b)
	c.Redirect(http.StatusSeeOther, "/runs/"+run.ID)
}

// handleAlertAck marks a single alert as acknowledged so it stops
// contributing to the navbar badge count.
func (s *Server) handleAlertAck(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" || len(id) > 128 {
		c.String(http.StatusBadRequest, "invalid id")
		return
	}
	// Alert IDs are RFC3339Nano-<bot>; character set is a strict subset
	// of what we already validate for run IDs (adds ':' and '.'). Keep
	// the check conservative: letters, digits, and a small punctuation set.
	for _, r := range id {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' || r == ':' || r == 'T' || r == 'Z'
		if !ok {
			c.String(http.StatusBadRequest, "invalid id")
			return
		}
	}
	if err := models.AckAlert(s.Cfg.BotsDir, id); err != nil {
		c.String(http.StatusInternalServerError, "ack: %v", err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/bots")
}

