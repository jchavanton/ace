// Copyright (C) 2026 Julien Chavanton <jchavanton@gmail.com>
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// APILogEntry is one row in the /api-docs log tab.
type APILogEntry struct {
	Time       time.Time
	Method     string
	Path       string
	Status     int
	DurationMs int64
	User       string
	ClientIP   string
	Error      string
}

// APILog is a thread-safe fixed-capacity ring buffer of API-call records.
// The buffer is in-memory only — operator-visible observability, not an
// audit log — so a restart wipes it.
type APILog struct {
	mu  sync.Mutex
	cap int
	buf []APILogEntry
}

func NewAPILog(capacity int) *APILog {
	if capacity <= 0 {
		capacity = 500
	}
	return &APILog{cap: capacity, buf: make([]APILogEntry, 0, capacity)}
}

func (l *APILog) append(e APILogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) < l.cap {
		l.buf = append(l.buf, e)
		return
	}
	// Full: drop oldest, append newest. Copy is O(cap) but cap is small
	// (500) and append rate is bounded by human/CI usage.
	copy(l.buf, l.buf[1:])
	l.buf[len(l.buf)-1] = e
}

// Snapshot returns a newest-first copy of the buffer so callers can
// iterate without holding the lock.
func (l *APILog) Snapshot() []APILogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]APILogEntry, len(l.buf))
	for i, e := range l.buf {
		out[len(l.buf)-1-i] = e
	}
	return out
}

// apiLogMiddleware records every request that passes through it. Register
// it only on /api/* routes so the ring buffer stays signal-dense.
func apiLogMiddleware(log *APILog) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		log.append(APILogEntry{
			Time:       start,
			Method:     c.Request.Method,
			Path:       c.Request.URL.Path,
			Status:     c.Writer.Status(),
			DurationMs: time.Since(start).Milliseconds(),
			User:       c.GetString(ctxUserKey),
			ClientIP:   c.ClientIP(),
			Error:      c.Errors.String(),
		})
	}
}
