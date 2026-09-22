package controller

import (
	"fmt"
	"net"
	"net/smtp"
	"strings"

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
