package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/jchavanton/ace/controller"
	"github.com/jchavanton/ace/models"
)

// handleAlerts renders the /alerts page: config form + list of firing
// bots + alert history + send-test button.
func (s *Server) handleAlerts(c *gin.Context) {
	cfg, err := models.LoadAlertConfig(s.Cfg.BotsDir)
	if err != nil {
		c.String(http.StatusInternalServerError, "load alert config: %v", err)
		return
	}
	alerts, err := models.ListAlerts(s.Cfg.BotsDir)
	if err != nil {
		c.String(http.StatusInternalServerError, "list alerts: %v", err)
		return
	}
	if len(alerts) > 200 {
		alerts = alerts[:200]
	}
	states, err := models.LoadAlertStates(s.Cfg.BotsDir)
	if err != nil {
		c.String(http.StatusInternalServerError, "load alert states: %v", err)
		return
	}
	// Firing view — pair the bot name with its last reason so operators
	// can triage without clicking through.
	type firing struct {
		Bot    string
		State  models.AlertState
	}
	var firingList []firing
	for name, st := range states {
		if st.State == "firing" {
			firingList = append(firingList, firing{Bot: name, State: st})
		}
	}
	// Effective alerts-retention (config-file override → flag default)
	// for the UI. -1 in the config means "explicitly disabled" and
	// renders as 0 in the effective value.
	alertsKeepEffective := s.Cfg.AlertsRetentionCount
	switch cfg.AlertsRetentionCount {
	case 0:
	case -1:
		alertsKeepEffective = 0
	default:
		alertsKeepEffective = cfg.AlertsRetentionCount
	}
	s.render(c, http.StatusOK, gin.H{
		"Title":                    "Alerts",
		"Page":                     "alerts",
		"ContentTemplate":          "content_alerts",
		"Config":                   cfg,
		"Firing":                   firingList,
		"Alerts":                   alerts,
		"TestMessage":              c.Query("test"),
		"AlertsRetentionEffective": alertsKeepEffective,
	})
}

// handleAlertConfigSave persists the SMTP config from the /alerts form.
// Empty SMTPHost is legal — clears out delivery and puts alerts back in
// UI-only mode. Retention/cleanup fields are preserved from the
// existing on-disk config (they live in the same file but a separate
// form on the same page).
func (s *Server) handleAlertConfigSave(c *gin.Context) {
	cfg, _ := models.LoadAlertConfig(s.Cfg.BotsDir)
	cfg.SMTPHost = strings.TrimSpace(c.PostForm("smtp_host"))
	cfg.SMTPUsername = strings.TrimSpace(c.PostForm("smtp_username"))
	cfg.SMTPPassword = c.PostForm("smtp_password")
	cfg.FromAddress = strings.TrimSpace(c.PostForm("from_address"))
	cfg.SMTPPort = 0
	if raw := strings.TrimSpace(c.PostForm("smtp_port")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 65535 {
			c.String(http.StatusBadRequest, "smtp_port: not a valid port")
			return
		}
		cfg.SMTPPort = n
	}
	cfg.ReminderIntervalHours = 0
	if raw := strings.TrimSpace(c.PostForm("reminder_interval_hours")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			c.String(http.StatusBadRequest, "reminder_interval_hours: must be >= 0")
			return
		}
		cfg.ReminderIntervalHours = n
	}
	cfg.Recipients = nil
	raw := c.PostForm("recipients")
	for _, r := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		r = strings.TrimSpace(r)
		if r != "" {
			cfg.Recipients = append(cfg.Recipients, r)
		}
	}
	if err := models.SaveAlertConfig(s.Cfg.BotsDir, cfg); err != nil {
		c.String(http.StatusInternalServerError, "save: %v", err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/alerts")
}

// handleAlertsRetentionSave persists the alerts.json cap from the
// Retention card on /alerts. Empty = use flag default, "0" = disabled
// (stored as -1), positive = row count. Cleanup for the runs dir is
// controlled on /runs, not here.
func (s *Server) handleAlertsRetentionSave(c *gin.Context) {
	cfg, _ := models.LoadAlertConfig(s.Cfg.BotsDir)
	if raw := strings.TrimSpace(c.PostForm("alerts_retention_count")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			c.String(http.StatusBadRequest, "alerts_retention_count: must be >= 0")
			return
		}
		if n == 0 {
			cfg.AlertsRetentionCount = -1
		} else {
			cfg.AlertsRetentionCount = n
		}
	} else {
		cfg.AlertsRetentionCount = 0
	}
	if err := models.SaveAlertConfig(s.Cfg.BotsDir, cfg); err != nil {
		c.String(http.StatusInternalServerError, "save: %v", err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/alerts")
}


// handleAlertTest sends a fixed "this is a test" message using the
// currently-saved config. Result is shown via ?test=ok|error=... on
// the /alerts page.
func (s *Server) handleAlertTest(c *gin.Context) {
	cfg, err := models.LoadAlertConfig(s.Cfg.BotsDir)
	if err != nil {
		c.Redirect(http.StatusSeeOther, "/alerts?test=error+load+config")
		return
	}
	if cfg.SMTPHost == "" {
		c.Redirect(http.StatusSeeOther, "/alerts?test=no+smtp+host+configured")
		return
	}
	err = controller.SendAlertEmail(cfg,
		"[ace TEST] alert delivery test",
		"This is a test message from ace's /alerts page. Delivery is working if you see this.\n")
	if err != nil {
		c.Redirect(http.StatusSeeOther, "/alerts?test=error:+"+err.Error())
		return
	}
	c.Redirect(http.StatusSeeOther, "/alerts?test=sent")
}
