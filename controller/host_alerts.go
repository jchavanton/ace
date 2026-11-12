// Copyright (C) 2026 Julien Chavanton <jchavanton@gmail.com>
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jchavanton/ace/config"
	"github.com/jchavanton/ace/models"
)

// HostMonitor polls prometheus for host-level metrics (CPU / mem /
// disk) at a fixed cadence and files alerts through the same
// AlertState/alerts.json machinery as bot alerts. Keys are
// "host:cpu", "host:mem", "host:disk" — reserved namespace so no
// bot name collides. Thresholds live on AlertConfig; a zero
// threshold disables that check (opt-in).
//
// Separate from Scheduler because it polls prometheus on a timer
// rather than reacting to run finish, and because its "subject"
// (the host itself) has no run.ID / scenario — forcing it through
// Scheduler.transition would mean nil-safing every helper there.
type HostMonitor struct {
	Cfg *config.Config

	mu   sync.Mutex
	done chan struct{}
}

// HostCheckInterval is how often the monitor polls prometheus. Short
// enough to catch a disk-fill in minutes, long enough that the
// prometheus hit is negligible. Not configurable — the operator can
// turn individual checks off via thresholds.
const HostCheckInterval = 30 * time.Second

// hostAlertKey → PromQL query. All three resolve to a scalar
// percentage so threshold comparison is straightforward.
var hostQueries = map[string]struct {
	Query string
	Label string // human-readable in email bodies / history reasons
}{
	"host:cpu":  {Query: `100 - (avg(irate(node_cpu_seconds_total{mode="idle"}[2m])) * 100)`, Label: "CPU busy"},
	"host:mem":  {Query: `(1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes) * 100`, Label: "memory used"},
	"host:disk": {Query: `(1 - node_filesystem_avail_bytes{mountpoint="/",fstype!~"tmpfs|overlay|squashfs"} / node_filesystem_size_bytes{mountpoint="/",fstype!~"tmpfs|overlay|squashfs"}) * 100`, Label: "root disk used"},
}

// Start spawns the monitor goroutine. Idempotent — a second call is
// a no-op. Exits only on Stop. When PrometheusURL is empty the
// monitor still runs but every check is a no-op; keeps the lifecycle
// simple (main doesn't need to branch on config).
func (h *HostMonitor) Start() {
	h.mu.Lock()
	if h.done != nil {
		h.mu.Unlock()
		return
	}
	h.done = make(chan struct{})
	done := h.done
	h.mu.Unlock()

	go func() {
		t := time.NewTicker(HostCheckInterval)
		defer t.Stop()
		// Immediate first check so a freshly-configured threshold doesn't
		// wait a full interval to take effect.
		h.tick()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				h.tick()
			}
		}
	}()
}

// Stop halts the goroutine. Idempotent.
func (h *HostMonitor) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done == nil {
		return
	}
	close(h.done)
	h.done = nil
}

func (h *HostMonitor) tick() {
	if h.Cfg.PrometheusURL == "" {
		return
	}
	cfg, err := models.LoadAlertConfig(h.Cfg.BotsDir)
	if err != nil {
		log.Printf("ace: host monitor: load alert config: %v", err)
		return
	}
	// Pull all three in one pass so transitions share a single
	// state-file read/write.
	type check struct {
		key       string
		threshold int
	}
	checks := []check{
		{"host:cpu", cfg.HostCPUThresholdPct},
		{"host:mem", cfg.HostMemThresholdPct},
		{"host:disk", cfg.HostDiskThresholdPct},
	}
	states, err := models.LoadAlertStates(h.Cfg.BotsDir)
	if err != nil {
		log.Printf("ace: host monitor: load states: %v", err)
		return
	}
	changed := false
	for _, c := range checks {
		q := hostQueries[c.key]
		if c.threshold <= 0 {
			// Check disabled. If a prior firing is on record, clear it
			// so the operator can turn things off cleanly.
			if st, ok := states[c.key]; ok && st.State == "firing" {
				st.State = "ok"
				st.LastReason = "check disabled"
				st.ConsecutiveFails = 0
				states[c.key] = st
				changed = true
				h.append(c.key, time.Now().UTC(), "cleared: check disabled")
			}
			continue
		}
		val, err := h.query(q.Query)
		if err != nil {
			// Transient prometheus error — log but don't flip state. A
			// persistent failure to poll shouldn't look the same as a
			// metric that recovered.
			log.Printf("ace: host monitor: query %s: %v", c.key, err)
			continue
		}
		if h.applyTransition(states, c.key, val, c.threshold, q.Label, cfg) {
			changed = true
		}
	}
	if changed {
		if err := models.SaveAlertStates(h.Cfg.BotsDir, states); err != nil {
			log.Printf("ace: host monitor: save states: %v", err)
		}
	}
}

// applyTransition mutates `states` in place and returns whether a
// state change occurred. Pulled out so the loop body stays readable.
// Mirrors Scheduler.transition's three-case shape (fail/first,
// fail/continued, recovered) but with the host-specific body.
func (h *HostMonitor) applyTransition(states map[string]models.AlertState, key string, value float64, threshold int, label string, cfg models.AlertConfig) bool {
	now := time.Now().UTC()
	prev := states[key]
	// Fire inclusive at the threshold — matches a sysadmin's reading
	// of "alert at 85%".
	breached := value >= float64(threshold)
	reason := fmt.Sprintf("%s %.1f%% >= %d%% threshold", label, value, threshold)
	recoveredReason := fmt.Sprintf("%s %.1f%% < %d%% threshold", label, value, threshold)

	next := prev
	switch {
	case breached && prev.State != "firing":
		next.State = "firing"
		next.LastAt = now
		next.LastReason = reason
		next.ConsecutiveFails = prev.ConsecutiveFails + 1
		next.LastNotifiedAt = now
		h.append(key, now, reason)
		h.send(cfg, key, reason, "firing")

	case breached && prev.State == "firing":
		next.LastAt = now
		next.LastReason = reason
		next.ConsecutiveFails = prev.ConsecutiveFails + 1
		if cfg.ReminderIntervalHours > 0 {
			if now.Sub(prev.LastNotifiedAt) >= time.Duration(cfg.ReminderIntervalHours)*time.Hour {
				next.LastNotifiedAt = now
				h.append(key, now, "reminder: "+reason)
				h.send(cfg, key, reason, "reminder")
			}
		}

	case !breached && prev.State == "firing":
		next.State = "ok"
		next.LastAt = now
		next.LastReason = ""
		next.ConsecutiveFails = 0
		next.LastNotifiedAt = now
		h.append(key, now, "recovered: "+recoveredReason)
		h.send(cfg, key, recoveredReason, "recovered")

	default:
		return false
	}
	states[key] = next
	return true
}

// query runs an instant query against the configured prometheus and
// returns the scalar value of the first sample. Returns an error if
// the response has no series — which for the host_* queries means
// node_exporter isn't being scraped yet.
func (h *HostMonitor) query(promQL string) (float64, error) {
	q := url.Values{}
	q.Set("query", promQL)
	q.Set("time", strconv.FormatInt(time.Now().Unix(), 10))
	reqURL := fmt.Sprintf("%s/api/v1/query?%s", h.Cfg.PrometheusURL, q.Encode())
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(reqURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	var pm struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value [2]interface{} `json:"value"`
			} `json:"result"`
		} `json:"data"`
		Error string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &pm); err != nil {
		return 0, err
	}
	if pm.Status != "success" {
		return 0, fmt.Errorf("prometheus: %s", pm.Error)
	}
	if len(pm.Data.Result) == 0 {
		return 0, fmt.Errorf("no series (node_exporter not scraped?)")
	}
	s, ok := pm.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("unexpected value type %T", pm.Data.Result[0].Value[1])
	}
	return strconv.ParseFloat(s, 64)
}

// append files one history row keyed by the synthetic host-check
// name. RunID / Scenario are empty — the template renders these rows
// without a run link.
func (h *HostMonitor) append(key string, at time.Time, reason string) {
	a := models.Alert{
		ID:     models.NewAlertID(at, key),
		Bot:    key,
		At:     at,
		Reason: reason,
	}
	if err := models.AppendAlert(h.Cfg.BotsDir, a); err != nil {
		log.Printf("ace: host monitor: append alert: %v", err)
	}
}

// send emits the email for a host-check transition. Separate from
// Scheduler.sendEmail because the body is run-free; shares subject
// prefixes with the bot path so filter rules still work.
func (h *HostMonitor) send(cfg models.AlertConfig, key, reason, transitionKind string) {
	if cfg.SMTPHost == "" {
		return
	}
	var prefix string
	switch transitionKind {
	case "firing":
		prefix = "[ace ALERT]"
	case "reminder":
		prefix = "[ace REMINDER]"
	case "recovered":
		prefix = "[ace RECOVERED]"
	default:
		prefix = "[ace]"
	}
	subject := fmt.Sprintf("%s %s", prefix, key)
	body := buildHostAlertBody(h.Cfg.PublicBaseURL, h.Cfg.Hostname, h.Cfg.DetectedPublicIP, key, reason)
	if err := SendAlertEmail(cfg, subject, body); err != nil {
		log.Printf("ace: host monitor: send email: %v", err)
		h.append(key, time.Now().UTC(), "email failed: "+err.Error())
	}
}

// buildHostAlertBody is the mailer.go counterpart for run-free host
// alerts. Short on purpose — the operator should land on /monitoring
// for the chart. Hostname + public IP are rendered when available so
// the recipient can tell which box fired the alert (relevant when a
// recipient list is shared across multiple ace deployments).
func buildHostAlertBody(publicBaseURL, hostname, publicIP, key, reason string) string {
	var b strings.Builder
	if hostname != "" || publicIP != "" {
		b.WriteString("Host: ")
		switch {
		case hostname != "" && publicIP != "":
			fmt.Fprintf(&b, "%s (%s)\n", hostname, publicIP)
		case hostname != "":
			fmt.Fprintf(&b, "%s\n", hostname)
		default:
			fmt.Fprintf(&b, "%s\n", publicIP)
		}
	}
	fmt.Fprintf(&b, "Host check: %s\nReason: %s\nTime: %s\n", key, reason, time.Now().UTC().Format(time.RFC3339))
	if publicBaseURL != "" {
		fmt.Fprintf(&b, "\n%s/monitoring\n", publicBaseURL)
	}
	return b.String()
}
