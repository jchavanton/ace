// Copyright (C) 2026 Julien Chavanton <jchavanton@gmail.com>
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jchavanton/ace/models"
)

// WebhookPoster emits run lifecycle events as HTTP JSON POSTs. Zero or
// nil = disabled; NewWebhookPoster returns nil when the configured URL
// is empty so callers can treat "no poster" and "disabled poster" the
// same way (both no-op).
//
// Delivery is fire-and-forget: each event is sent on its own goroutine,
// failures are logged but never surfaced back to the run. This matches
// the AddOnFinish contract (hooks are best-effort observability) and
// keeps a slow or broken receiver from stalling voip_patrol.
type WebhookPoster struct {
	URL     string
	Secret  []byte // HMAC-SHA256 key; nil = no signature header
	Timeout time.Duration

	client *http.Client

	// outstanding tracks in-flight posts so tests (and future graceful
	// shutdown) can wait for them to drain. Not consulted by production
	// code paths — the hook is best-effort, callers never block on it.
	outstanding sync.WaitGroup
}

// NewWebhookPoster returns a poster for url, or nil when url is empty.
// A nil *WebhookPoster is safe to call methods on — they're all no-ops.
func NewWebhookPoster(url, secret string, timeout time.Duration) *WebhookPoster {
	if url == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	p := &WebhookPoster{
		URL:     url,
		Timeout: timeout,
		client:  &http.Client{Timeout: timeout},
	}
	if secret != "" {
		p.Secret = []byte(secret)
	}
	return p
}

// callEvent is the payload for a per-call result as voip_patrol writes
// one line to results.json. Emitted by the runner's tailer while the
// run is still in flight.
type callEvent struct {
	Event       string             `json:"event"` // always "call"
	RunID       string             `json:"run_id"`
	Scenario    string             `json:"scenario"`
	StartedBy   string             `json:"started_by,omitempty"`
	TriggeredBy string             `json:"triggered_by,omitempty"`
	Call        models.CallResult  `json:"call"`
}

// runFinishedEvent is the payload for the terminal event, emitted once
// after run.json has been persisted with status != "running". The full
// Run (including Calls and Aggregate) is embedded so consumers can
// publish both a per-call stream and a summary from the same source.
type runFinishedEvent struct {
	Event  string      `json:"event"` // always "run.finished"
	RunID  string      `json:"run_id"`
	Status string      `json:"status"`         // done | error | stopped
	Passed *bool       `json:"passed"`         // nil-safe; nil only for malformed runs
	Run    *models.Run `json:"run"`
}

// PostCall sends a "call" event for a single row of results.json.
// Nil receiver = no-op (webhook disabled).
func (p *WebhookPoster) PostCall(run *models.Run, call models.CallResult) {
	if p == nil {
		return
	}
	ev := callEvent{
		Event:       "call",
		RunID:       run.ID,
		Scenario:    run.Scenario,
		StartedBy:   run.StartedBy,
		TriggeredBy: run.TriggeredBy,
		Call:        call,
	}
	p.send(ev)
}

// PostRunFinished sends a "run.finished" event with the full Run.
// Intended for registration via Runner.AddOnFinish. Nil receiver =
// no-op.
func (p *WebhookPoster) PostRunFinished(run *models.Run) {
	if p == nil {
		return
	}
	ev := runFinishedEvent{
		Event:  "run.finished",
		RunID:  run.ID,
		Status: run.Status,
		Passed: runPassedBool(run),
		Run:    run,
	}
	p.send(ev)
}

// Wait blocks until all in-flight posts have returned. Tests use this
// to assert on the receiver; production code doesn't call it.
func (p *WebhookPoster) Wait() {
	if p == nil {
		return
	}
	p.outstanding.Wait()
}

// send serializes payload and POSTs it on a background goroutine.
func (p *WebhookPoster) send(payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("webhook: marshal: %v", err)
		return
	}
	p.outstanding.Add(1)
	go func() {
		defer p.outstanding.Done()
		if err := p.post(body); err != nil {
			log.Printf("webhook POST %s: %v", p.URL, err)
			return
		}
		log.Printf("webhook POST %s ok (%d bytes)", p.URL, len(body))
	}()
}

func (p *WebhookPoster) post(body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), p.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ace/webhook")
	if len(p.Secret) > 0 {
		mac := hmac.New(sha256.New, p.Secret)
		mac.Write(body)
		req.Header.Set("X-Ace-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused by keep-alive. Cap the read
	// so a chatty receiver can't fill our memory.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// runPassedBool mirrors handlers/api.go's runPassed logic without
// pulling in the handlers package (import cycle). Returns nil only for
// a run.Status we don't recognize — all terminal statuses produce a
// bool so the receiver can gate on it.
func runPassedBool(run *models.Run) *bool {
	if run == nil {
		return nil
	}
	switch run.Status {
	case "done":
		passed := len(run.Calls) > 0
		for _, c := range run.Calls {
			if c.Result != "" && !strings.EqualFold(c.Result, "PASS") {
				passed = false
			}
		}
		return &passed
	case "error", "stopped":
		b := false
		return &b
	}
	return nil
}
