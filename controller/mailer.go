package controller

import (
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/jchavanton/ace/models"
)

// SendAlertEmail delivers one alert message via SMTP. Returns nil when
// the config is empty (no SMTP host configured) so callers don't need
// to gate on that themselves — this is the "email off, keep going"
// path. Any real send error is returned so the caller can log and file
// a delivery-failed record if it wants to.
//
// Uses net/smtp's PlainAuth over STARTTLS when a username is set;
// falls back to no-auth when SMTPUsername is blank (common for
// localhost relays). Port defaults to 587 (submission) when unset.
func SendAlertEmail(cfg models.AlertConfig, subject, body string) error {
	if cfg.SMTPHost == "" {
		return nil
	}
	if len(cfg.Recipients) == 0 {
		return fmt.Errorf("no recipients configured")
	}
	from := cfg.FromAddress
	if from == "" {
		from = cfg.SMTPUsername
	}
	if from == "" {
		return fmt.Errorf("no from address configured")
	}
	port := cfg.SMTPPort
	if port == 0 {
		port = 587
	}
	addr := net.JoinHostPort(cfg.SMTPHost, fmt.Sprintf("%d", port))

	var auth smtp.Auth
	if cfg.SMTPUsername != "" {
		auth = smtp.PlainAuth("", cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPHost)
	}

	msg := buildMessage(from, cfg.Recipients, subject, body)
	return smtp.SendMail(addr, auth, from, cfg.Recipients, msg)
}

// buildAlertBody assembles the plain-text alert email body — the same
// data the /runs/<id> detail page shows, so the on-call recipient has
// enough to triage without opening a browser. Layout matches the UI
// sections: header, aggregate, per-call breakdown (only failing calls
// when the run has many results, keeping the mail readable), and a
// link to the run detail page when a base URL is configured.
func buildAlertBody(publicBaseURL, botName string, run *models.Run, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Bot: %s\n", botName)
	fmt.Fprintf(&b, "Scenario: %s\n", run.Scenario)
	fmt.Fprintf(&b, "Run: %s\n", run.ID)
	fmt.Fprintf(&b, "Status: %s\n", run.Status)
	fmt.Fprintf(&b, "Reason: %s\n", reason)
	fmt.Fprintf(&b, "Started: %s\n", run.StartedAt.Format(time.RFC3339))
	if !run.FinishedAt.IsZero() {
		fmt.Fprintf(&b, "Finished: %s\n", run.FinishedAt.Format(time.RFC3339))
		fmt.Fprintf(&b, "Duration: %s\n", run.FinishedAt.Sub(run.StartedAt).Round(time.Second))
	}
	fmt.Fprintf(&b, "Exit code: %d\n", run.ExitCode)
	if run.SIPPort > 0 {
		fmt.Fprintf(&b, "SIP: %d", run.SIPPort)
		if run.Transport != "" {
			fmt.Fprintf(&b, " (%s)", run.Transport)
		}
		b.WriteString("\n")
	}
	if run.RTPPortStart > 0 {
		fmt.Fprintf(&b, "RTP: %d-%d\n", run.RTPPortStart, run.RTPPortEnd)
	}
	if run.PublicAddress != "" {
		fmt.Fprintf(&b, "Public: %s\n", run.PublicAddress)
	}
	if run.Error != "" {
		fmt.Fprintf(&b, "Error: %s\n", run.Error)
	}

	if run.Aggregate.Total > 0 {
		agg := run.Aggregate
		b.WriteString("\n-- Aggregate --\n")
		fmt.Fprintf(&b, "Calls: %d pass / %d total", agg.Pass, agg.Total)
		if agg.Fail > 0 {
			fmt.Fprintf(&b, " (%d failed)", agg.Fail)
		}
		b.WriteString("\n")
		if agg.Invite200P50 > 0 || agg.Invite200P95 > 0 {
			fmt.Fprintf(&b, "Invite→200: %d ms p50 / %d ms p95\n", agg.Invite200P50, agg.Invite200P95)
		}
		if agg.MOSAvgRx > 0 || agg.MOSAvgTx > 0 {
			fmt.Fprintf(&b, "MOS avg: %.2f Rx / %.2f Tx\n", agg.MOSAvgRx, agg.MOSAvgTx)
		}
		if agg.RTTAvgMs > 0 {
			fmt.Fprintf(&b, "RTT avg: %d ms\n", agg.RTTAvgMs)
		}
		if agg.PacketsLossTx > 0 {
			fmt.Fprintf(&b, "Tx packets lost: %d\n", agg.PacketsLossTx)
		}
	}

	if len(run.Calls) > 0 {
		// Filter to failing calls when the total is large — a 100-call
		// scenario would blow up the email; the operator can click
		// through for the full table.
		toShow := run.Calls
		filtered := false
		if len(run.Calls) > 10 && run.Aggregate.Fail > 0 && run.Aggregate.Fail < len(run.Calls) {
			toShow = toShow[:0]
			for _, c := range run.Calls {
				if !strings.EqualFold(c.Result, "PASS") {
					toShow = append(toShow, c)
				}
			}
			filtered = true
		}
		b.WriteString("\n-- Calls")
		if filtered {
			fmt.Fprintf(&b, " (%d failing shown of %d total)", len(toShow), len(run.Calls))
		}
		b.WriteString(" --\n")
		for i, c := range toShow {
			fmt.Fprintf(&b, "#%d %s cause=%d dur=%ds",
				i, c.Result, c.CauseCode, c.Duration)
			if c.Reason != "" {
				fmt.Fprintf(&b, " reason=%q", c.Reason)
			}
			if c.SIPLatency.Invite200Ms > 0 {
				fmt.Fprintf(&b, " 200=%dms", c.SIPLatency.Invite200Ms)
			}
			for _, rs := range c.RTPStats {
				if rs.Rx.MosLQ > 0 || rs.Tx.MosLQ > 0 {
					fmt.Fprintf(&b, " MOS=%.2f/%.2f", rs.Rx.MosLQ, rs.Tx.MosLQ)
				}
			}
			if c.CallID != "" {
				fmt.Fprintf(&b, "\n    Call-ID: %s", c.CallID)
			}
			b.WriteString("\n")
		}
	}

	if publicBaseURL != "" {
		base := strings.TrimRight(publicBaseURL, "/")
		fmt.Fprintf(&b, "\n%s/runs/%s\n", base, run.ID)
	}
	return b.String()
}

// buildMessage assembles a minimal RFC 5322 message. We deliberately
// don't set MIME parts — the body is plain text and the UI keeps
// bot alert bodies short (subject + one paragraph + run link).
func buildMessage(from string, to []string, subject, body string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}
