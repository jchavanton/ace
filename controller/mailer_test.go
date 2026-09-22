package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/jchavanton/ace/models"
)

// TestBuildAlertBody checks that the enriched email body contains the
// same headline fields the /runs/<id> UI shows: aggregate, per-call
// row for a failing call, and the click-through link when a base URL
// is provided.
func TestBuildAlertBody(t *testing.T) {
	run := &models.Run{
		ID:            "20260922-214009-sbc_dev_julien",
		Scenario:      "sbc_dev_julien",
		Status:        "done",
		StartedAt:     time.Date(2026, 9, 22, 21, 40, 9, 0, time.UTC),
		FinishedAt:    time.Date(2026, 9, 22, 21, 40, 11, 0, time.UTC),
		SIPPort:       5093,
		RTPPortStart:  4000,
		RTPPortEnd:    14000,
		PublicAddress: "24.122.254.8",
		ExitCode:      0,
		Aggregate: models.Aggregate{
			Total:        1,
			Pass:         0,
			Fail:         1,
			Invite200P50: 42,
			Invite200P95: 42,
			MOSAvgRx:     3.5,
			MOSAvgTx:     3.6,
			RTTAvgMs:     18,
		},
		Calls: []models.CallResult{
			{
				Result:     "FAIL",
				CauseCode:  486,
				Duration:   0,
				Reason:     "Busy Here",
				CallID:     "abc@example.com",
				SIPLatency: models.SIPLatency{Invite200Ms: 42},
			},
		},
	}
	body := buildAlertBody("https://ace.example.com", "dev", run, "FAIL 1/1")

	wants := []string{
		"Bot: dev",
		"Scenario: sbc_dev_julien",
		"Run: 20260922-214009-sbc_dev_julien",
		"Reason: FAIL 1/1",
		"Duration: 2s",
		"SIP: 5093",
		"RTP: 4000-14000",
		"Public: 24.122.254.8",
		"-- Aggregate --",
		"Calls: 0 pass / 1 total (1 failed)",
		"Invite→200: 42 ms p50 / 42 ms p95",
		"MOS avg: 3.50 Rx / 3.60 Tx",
		"RTT avg: 18 ms",
		"-- Calls --",
		"#0 FAIL cause=486",
		`reason="Busy Here"`,
		"200=42ms",
		"Call-ID: abc@example.com",
		"https://ace.example.com/runs/20260922-214009-sbc_dev_julien",
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("body missing %q\n--- body ---\n%s", w, body)
		}
	}
	// Keep a sample of the rendered body in test output when -v is on
	// so future changes are easy to eyeball.
	t.Logf("sample rendered body:\n%s", body)
}

// TestBuildAlertBodyNoLink verifies the link is omitted when the base
// URL is empty (the LAN deploy case where there's no external hostname).
func TestBuildAlertBodyNoLink(t *testing.T) {
	run := &models.Run{ID: "x", Scenario: "s", Status: "done"}
	body := buildAlertBody("", "b", run, "recovered")
	if strings.Contains(body, "http") {
		t.Errorf("body should not contain a URL when base is empty:\n%s", body)
	}
}
