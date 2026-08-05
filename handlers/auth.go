package handlers

import (
	"bufio"
	"crypto/subtle"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// htpasswd holds a snapshot of bcrypt-hashed credentials from disk. The
// file is re-read at most once per reloadInterval so operators can add
// users via ansible without restarting ace. Callers only touch this
// through basicAuth.
//
// Format on disk: standard Apache htpasswd, bcrypt only ($2y$ / $2a$ /
// $2b$). One user per line: `username:$2y$05$...`. Anything else (MD5,
// SHA1, plain) is rejected at load — bcrypt is the only hash Go's
// x/crypto supports out of the box, and mixing weaker schemes would be
// a footgun.
type htpasswd struct {
	path     string
	mu       sync.RWMutex
	users    map[string][]byte // username -> bcrypt hash
	loadedAt time.Time
}

// reloadInterval bounds how often we re-stat the file. 10s is fine — a
// deploy that adds a user takes seconds to propagate, and we save a
// stat/read on every request.
const reloadInterval = 10 * time.Second

func newHtpasswd(path string) *htpasswd { return &htpasswd{path: path} }

// verify returns true iff (user, pass) matches an entry in the file.
// Constant-time on the username lookup miss to avoid trivially leaking
// which usernames exist; bcrypt.CompareHashAndPassword is already
// constant-time on the hash comparison.
func (h *htpasswd) verify(user, pass string) bool {
	h.maybeReload()
	h.mu.RLock()
	hash, ok := h.users[user]
	h.mu.RUnlock()
	if !ok {
		// Run bcrypt against a dummy hash so the timing of unknown-user
		// vs. wrong-password is indistinguishable. The dummy hash is a
		// valid bcrypt of an empty string; the compare always fails.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(pass))
		return false
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(pass)) == nil
}

// dummyHash is a valid bcrypt hash of an arbitrary string, used only
// for timing padding when the requested user doesn't exist.
var dummyHash = []byte("$2a$10$abcdefghijklmnopqrstuu.NwZBAO4qXcRvI7fkg6EiuiEz3Wu6gy")

func (h *htpasswd) maybeReload() {
	h.mu.RLock()
	fresh := time.Since(h.loadedAt) < reloadInterval
	h.mu.RUnlock()
	if fresh {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Double-check under the write lock — another goroutine may have
	// reloaded while we were waiting.
	if time.Since(h.loadedAt) < reloadInterval {
		return
	}
	users, err := readHtpasswd(h.path)
	if err != nil {
		// Keep serving the last-known-good set. A missing/corrupt file
		// should not turn every request into a 401 — the operator will
		// notice via logs and fix the file.
		h.loadedAt = time.Now()
		return
	}
	h.users = users
	h.loadedAt = time.Now()
}

func readHtpasswd(path string) (map[string][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make(map[string][]byte)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		user, hash, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		// Reject non-bcrypt entries silently — logging the file's
		// contents would leak hashes into stdout.
		if !strings.HasPrefix(hash, "$2y$") && !strings.HasPrefix(hash, "$2a$") && !strings.HasPrefix(hash, "$2b$") {
			continue
		}
		out[user] = []byte(hash)
	}
	return out, sc.Err()
}

// ctxUserKey is the gin context key the middleware stamps the
// authenticated username onto. handleRun reads it via c.GetString.
const ctxUserKey = "ace.user"

// basicAuth returns a gin middleware that enforces HTTP Basic on every
// request. When path is empty the middleware is a no-op — LAN
// deployments without auth get zero overhead and no behavior change.
func basicAuth(path string) gin.HandlerFunc {
	if path == "" {
		return func(c *gin.Context) { c.Next() }
	}
	store := newHtpasswd(path)
	return func(c *gin.Context) {
		user, pass, ok := c.Request.BasicAuth()
		// subtle.ConstantTimeCompare guards against a caller sending an
		// empty user with a valid password (or vice versa) trying to
		// short-circuit; BasicAuth already parses both, but explicit is
		// safer than relying on ok.
		if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(user)) != 1 {
			challenge(c)
			return
		}
		if !store.verify(user, pass) {
			challenge(c)
			return
		}
		c.Set(ctxUserKey, user)
		c.Next()
	}
}

func challenge(c *gin.Context) {
	c.Header("WWW-Authenticate", `Basic realm="ace"`)
	c.AbortWithStatus(http.StatusUnauthorized)
}
