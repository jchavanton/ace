package handlers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/jchavanton/ace/controller"
	"github.com/jchavanton/ace/models"
)

// handleFirewall renders the firewall page: current rule set plus the
// interface dropdown options. Read-only load errors surface as a banner
// on the page rather than a 500, so the operator can still add rules
// even if firewall.json is temporarily missing.
func (s *Server) handleFirewall(c *gin.Context) {
	if s.Firewall == nil {
		c.String(http.StatusNotFound, "firewall management disabled (start ace with -firewall-state-dir to enable)")
		return
	}
	cfg, loadErr := s.Firewall.Load()
	msg := c.Query("msg")
	errMsg := c.Query("err")
	c.HTML(http.StatusOK, "layout", gin.H{
		"Title":           "Firewall",
		"Page":            "firewall",
		"ContentTemplate": "content_firewall",
		"Rules":           cfg.Rules,
		"Interfaces":      controller.InterfaceNames(),
		"Chain":           s.Firewall.Chain,
		"LoadError": func() string {
			if loadErr != nil {
				return loadErr.Error()
			}
			return ""
		}(),
		"Message": msg,
		"Error":   errMsg,
	})
}

// handleFirewallSave parses the whole rule set out of the form and
// hands it to the controller for save + apply in one go. The form
// submits parallel arrays (cidr[], transport[], port[], iface[],
// comment[]); rows with a blank CIDR are dropped so operators can
// leave partial rows without failing the save.
func (s *Server) handleFirewallSave(c *gin.Context) {
	if s.Firewall == nil {
		c.String(http.StatusNotFound, "firewall management disabled")
		return
	}
	form, err := c.MultipartForm()
	if err != nil {
		// Fall back to URL-encoded form parsing — the template submits
		// as application/x-www-form-urlencoded by default.
		if err := c.Request.ParseForm(); err != nil {
			c.String(http.StatusBadRequest, "parse form: %v", err)
			return
		}
		form = nil
	}
	getSlice := func(key string) []string {
		if form != nil {
			if v, ok := form.Value[key]; ok {
				return v
			}
			return nil
		}
		return c.Request.PostForm[key]
	}
	cidrs := getSlice("cidr")
	transports := getSlice("transport")
	ports := getSlice("port")
	ifaces := getSlice("iface")
	actions := getSlice("action")
	comments := getSlice("comment")

	n := len(cidrs)
	if len(transports) > n {
		n = len(transports)
	}
	rules := make([]models.FirewallRule, 0, n)
	for i := 0; i < n; i++ {
		cidr := ""
		if i < len(cidrs) {
			cidr = strings.TrimSpace(cidrs[i])
		}
		if cidr == "" {
			continue
		}
		r := models.FirewallRule{CIDR: cidr}
		if i < len(transports) {
			r.Transport = transports[i]
		}
		if i < len(ports) {
			r.Port = ports[i]
		}
		if i < len(ifaces) {
			r.Interface = ifaces[i]
		}
		if i < len(actions) {
			r.Action = actions[i]
		}
		if i < len(comments) {
			r.Comment = comments[i]
		}
		rules = append(rules, r)
	}

	cfg := models.FirewallConfig{Rules: rules}
	if err := s.Firewall.SaveAndApply(cfg); err != nil {
		// Round-trip the error through a query param so the redirect
		// pattern is preserved and the operator sees the exact iptables
		// output. gin escapes it into the URL.
		c.Redirect(http.StatusSeeOther, "/firewall?err="+urlQueryEscape(err.Error()))
		return
	}
	c.Redirect(http.StatusSeeOther, "/firewall?msg="+urlQueryEscape("saved and applied"))
}

// urlQueryEscape is a minimal escaper — gin doesn't expose a helper and
// pulling net/url just for this is wasteful. Handles the characters we
// actually emit in error messages.
func urlQueryEscape(s string) string {
	r := strings.NewReplacer(
		"%", "%25",
		" ", "%20",
		"\n", "%0A",
		"\r", "",
		"&", "%26",
		"#", "%23",
		"?", "%3F",
		"+", "%2B",
	)
	return r.Replace(s)
}
