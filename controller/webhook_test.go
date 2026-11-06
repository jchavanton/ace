// Copyright (C) 2026 Julien Chavanton <jchavanton@gmail.com>
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jchavanton/ace/models"
)

func TestWebhookPoster_Disabled(t *testing.T) {
	// Empty URL → nil poster; all methods are safe no-ops. The hooks the
	// runner registers are called unconditionally per-event, so this is
	// a correctness-critical property.
	p := NewWebhookPoster("", "", 0)
	if p != nil {
		t.Fatalf("expected nil poster for empty URL, got %+v", p)
	}
	p.PostCall(&models.Run{}, models.CallResult{})
	p.PostRunFinished(&models.Run{})
	p.Wait()
}

func TestWebhookPoster_PostsCallAndFinish(t *testing.T) {
	type received struct {
		event string
		body  []byte
	}
	var (
		mu   sync.Mutex
		list []received
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var head struct{ Event string `json:"event"` }
		_ = json.Unmarshal(b, &head)
		mu.Lock()
		list = append(list, received{event: head.Event, body: b})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := NewWebhookPoster(srv.URL, "", time.Second)
	run := &models.Run{ID: "r1", Scenario: "s", Status: "done", Calls: []models.CallResult{{Result: "PASS"}}}
	p.PostCall(run, run.Calls[0])
	p.PostRunFinished(run)
	p.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(list) != 2 {
		t.Fatalf("expected 2 events, got %d", len(list))
	}
	// Order between the two POSTs isn't guaranteed (fire-and-forget on
	// goroutines), so check both are present rather than their indices.
	events := map[string]bool{list[0].event: true, list[1].event: true}
	if !events["call"] || !events["run.finished"] {
		t.Fatalf("missing event: %v", events)
	}

	// Spot-check the finished payload carries the full run including Calls.
	for _, r := range list {
		if r.event != "run.finished" {
			continue
		}
		var ev runFinishedEvent
		if err := json.Unmarshal(r.body, &ev); err != nil {
			t.Fatalf("decode finished: %v", err)
		}
		if ev.RunID != "r1" || ev.Status != "done" {
			t.Fatalf("wrong finished fields: %+v", ev)
		}
		if ev.Passed == nil || !*ev.Passed {
			t.Fatalf("expected passed=true, got %+v", ev.Passed)
		}
		if len(ev.Run.Calls) != 1 {
			t.Fatalf("expected 1 call in Run, got %d", len(ev.Run.Calls))
		}
	}
}

func TestWebhookPoster_HMACSignature(t *testing.T) {
	const secret = "s3cret"
	var (
		gotSig  string
		gotBody []byte
		done    = make(chan struct{})
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Ace-Signature")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		close(done)
	}))
	defer srv.Close()

	p := NewWebhookPoster(srv.URL, secret, time.Second)
	p.PostRunFinished(&models.Run{ID: "x", Status: "done", Calls: []models.CallResult{{Result: "PASS"}}})
	<-done
	p.Wait()

	if !strings.HasPrefix(gotSig, "sha256=") {
		t.Fatalf("missing sha256 prefix: %q", gotSig)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(gotBody)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Fatalf("sig mismatch:\n got  %s\n want %s", gotSig, want)
	}
}

func TestWebhookPoster_ErrorsAreSwallowed(t *testing.T) {
	// 500 from the receiver must not panic or wedge Wait(). This is
	// what guarantees the webhook can never fail a run.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := NewWebhookPoster(srv.URL, "", 500*time.Millisecond)
	p.PostCall(&models.Run{ID: "r"}, models.CallResult{})
	p.Wait()
}
