package server

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// failoverTargetHealth is the per-target health snapshot exposed through the
// model listing so the web UI can show the state of a failover chain.
type failoverTargetHealth struct {
	Target    string `json:"target"`
	Healthy   bool   `json:"healthy"`
	RetryAt   int64  `json:"retryAt,omitempty"`
	LastError string `json:"lastError,omitempty"`
}

// selectorFailoverState tracks the circuit breaker for one failover selector.
// A target that fails is skipped (unhealthy) until its cooldown expires; a
// successful request through a target clears the mark immediately.
type selectorFailoverState struct {
	mu        sync.Mutex
	cooldown  time.Duration
	targets   []string
	unhealthy map[string]time.Time // target -> healthy again no earlier than
	lastErr   map[string]string
}

type selectorFailoverTracker struct {
	states map[string]*selectorFailoverState
}

func newSelectorFailoverTracker(cfg config.Config) *selectorFailoverTracker {
	tracker := &selectorFailoverTracker{states: make(map[string]*selectorFailoverState)}
	for selectorID, selector := range cfg.Selectors {
		if selector.Strategy != config.SelectorStrategyFailover {
			continue
		}
		cooldown := selector.Settings.CooldownDuration()
		if cooldown <= 0 {
			cooldown = config.DefaultSelectorFailoverCooldown
		}
		tracker.states[selectorID] = &selectorFailoverState{
			cooldown:  cooldown,
			targets:   append([]string(nil), selector.Targets...),
			unhealthy: make(map[string]time.Time),
			lastErr:   make(map[string]string),
		}
	}
	return tracker
}

func (t *selectorFailoverTracker) state(selectorID string) *selectorFailoverState {
	if t == nil {
		return nil
	}
	return t.states[selectorID]
}

// eligibleTargets returns the targets to try, in configured order, skipping
// those still in cooldown. When every target is in cooldown the full list is
// returned: the cooldown exists to spare latency on a known-dead primary, not
// to take a fully-degraded chain offline for the whole cooldown window.
func (s *selectorFailoverState) eligibleTargets(now time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	eligible := make([]string, 0, len(s.targets))
	for _, target := range s.targets {
		if retryAt, unhealthy := s.unhealthy[target]; !unhealthy || now.After(retryAt) {
			eligible = append(eligible, target)
		}
	}
	if len(eligible) == 0 {
		return append([]string(nil), s.targets...)
	}
	return eligible
}

// markFailed puts target into cooldown and returns how long it will be skipped.
func (s *selectorFailoverState) markFailed(target, reason string, now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unhealthy[target] = now.Add(s.cooldown)
	s.lastErr[target] = reason
	return s.cooldown
}

// markHealthy re-arms a target immediately after a successful request.
func (s *selectorFailoverState) markHealthy(target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.unhealthy, target)
	delete(s.lastErr, target)
}

// health returns the per-target health snapshot for the models listing.
func (t *selectorFailoverTracker) health(selectorID string, now time.Time) []failoverTargetHealth {
	state := t.state(selectorID)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()

	health := make([]failoverTargetHealth, 0, len(state.targets))
	for _, target := range state.targets {
		entry := failoverTargetHealth{Target: target, Healthy: true}
		if retryAt, unhealthy := state.unhealthy[target]; unhealthy {
			if now.After(retryAt) {
				// The cooldown has expired: report healthy again. The
				// entry is dropped so the next failure gets a fresh window.
				delete(state.unhealthy, target)
				delete(state.lastErr, target)
			} else {
				entry.Healthy = false
				entry.RetryAt = retryAt.Unix()
				entry.LastError = state.lastErr[target]
			}
		}
		health = append(health, entry)
	}
	return health
}

// failoverCaptureWriter buffers a downstream handler's response until its
// status is known. A 2xx response is committed to the real writer immediately
// (streaming responses pass through byte for byte from then on); anything
// else is held in memory so the failover loop can discard it and retry the
// next target, or flush it to the client when the last target failed.
//
// It implements swaputil.StatusMarker so the existing recorder chain (peer
// proxy error handler, client-closed sentinel) keeps working with this writer
// at the bottom of the stack.
type failoverCaptureWriter struct {
	dst http.ResponseWriter

	committed bool
	status    int // buffered non-2xx status when !committed
	header    http.Header
	body      bytes.Buffer
}

func (c *failoverCaptureWriter) Header() http.Header {
	if c.committed {
		return c.dst.Header()
	}
	if c.header == nil {
		c.header = make(http.Header)
	}
	return c.header
}

// commit forwards the buffered headers and the status to the real writer and
// switches the writer into pass-through mode.
func (c *failoverCaptureWriter) commit(status int) {
	dstHeader := c.dst.Header()
	for key, values := range c.Header() {
		dstHeader[key] = values
	}
	c.committed = true
	c.dst.WriteHeader(status)
}

func (c *failoverCaptureWriter) WriteHeader(code int) {
	if c.committed || c.status != 0 {
		// Superfluous WriteHeader, or a 2xx after a buffered error.
		return
	}
	if code < 200 {
		return // informational; the real response status is still to come
	}
	if code < 300 {
		c.commit(code)
		return
	}
	c.status = code
}

func (c *failoverCaptureWriter) Write(p []byte) (int, error) {
	if c.committed {
		return c.dst.Write(p)
	}
	if c.status == 0 {
		// Implicit 200: the first byte is the point of no return.
		c.commit(http.StatusOK)
		return c.dst.Write(p)
	}
	return c.body.Write(p)
}

func (c *failoverCaptureWriter) Flush() {
	if !c.committed {
		if c.status != 0 {
			// A buffered error response has nothing to stream.
			return
		}
		// Flushing before any status commits the implicit 200, exactly
		// as it would against the real writer.
		c.commit(http.StatusOK)
	}
	if flusher, ok := c.dst.(http.Flusher); ok {
		flusher.Flush()
	}
}

// MarkStatus records a status without writing anything, so the failover loop
// can inspect it (e.g. the 499 client-closed sentinel).
func (c *failoverCaptureWriter) MarkStatus(code int) {
	if c.committed || c.status == 0 {
		c.status = code
	}
}

func (c *failoverCaptureWriter) WroteHeader() bool {
	return c.committed || c.status != 0
}

func (c *failoverCaptureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := c.dst.(http.Hijacker); ok && c.committed {
		return hijacker.Hijack()
	}
	return nil, nil, errors.New("failover: cannot hijack an uncommitted response")
}

// flushBuffered forwards a held non-2xx response to the client. It is used
// when every target failed, so the client sees the LAST target's real error
// rather than a generic one.
func (c *failoverCaptureWriter) flushBuffered() {
	if c.committed {
		return
	}
	if c.status == 0 {
		c.status = http.StatusBadGateway
	}
	dstHeader := c.dst.Header()
	for key, values := range c.header {
		dstHeader[key] = values
	}
	c.dst.WriteHeader(c.status)
	c.body.WriteTo(c.dst)
}

// failoverResponseSummary is the one-line reason recorded for a failed target.
func failoverResponseSummary(c *failoverCaptureWriter) string {
	if c.status == 0 {
		return "no response"
	}
	summary := fmt.Sprintf("HTTP %d", c.status)
	if snippet := bytes.TrimSpace(c.body.Bytes()); len(snippet) > 0 {
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		summary += fmt.Sprintf(" %q", snippet)
	}
	return summary
}

// serveFailoverSelector runs one client request through a failover chain:
// targets are tried strictly in listed order; a target that fails before the
// first response byte is forwarded is retried against the next target.
func serveFailoverSelector(s *Server, next http.Handler, w http.ResponseWriter, r *http.Request, selectorID string, tracker *selectorFailoverTracker) {
	log := s.logs.ProxyLogs
	state := tracker.state(selectorID)
	if state == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, fmt.Sprintf("failover selector %q is not configured", selectorID))
		return
	}

	targets := state.eligibleTargets(time.Now())

	// Websocket upgrades cannot be buffered or replayed, so they get the
	// first eligible target without failover.
	if swaputil.IsWebSocketUpgrade(r) {
		target := targets[0]
		attempt, err := newFailoverAttempt(r, nil, selectorID, target)
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
			return
		}
		log.Debugf("selector: id=%s strategy=failover target=%s (websocket, no failover)", selectorID, target)
		next.ServeHTTP(w, withSelectorContext(attempt, selectorID))
		return
	}

	// Snapshot the request body once so each attempt can replay it.
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "could not read request body")
			return
		}
		r.Body.Close()
	}
	if len(body) == 0 {
		body = nil
	}

	var lastCapture *failoverCaptureWriter
	for i, target := range targets {
		// The client is gone: there is nobody to fail over for.
		if swaputil.ClientContext(r.Context()).Err() != nil {
			return
		}

		attempt, err := newFailoverAttempt(r, body, selectorID, target)
		if err != nil {
			reason := fmt.Sprintf("rewrite failed: %v", err)
			cooldown := state.markFailed(target, reason, time.Now())
			log.Warnf("selector=%s: failover: target=%s failed (%s); unhealthy for %s", selectorID, target, reason, cooldown)
			continue
		}

		capture := &failoverCaptureWriter{dst: w}
		next.ServeHTTP(capture, withSelectorContext(attempt, selectorID))

		if capture.committed {
			// A 2xx was forwarded to the client; the target is healthy
			// and no failover is possible from here on.
			state.markHealthy(target)
			return
		}

		if capture.status == swaputil.StatusClientClosedRequest || swaputil.ClientContext(r.Context()).Err() != nil {
			// The client disconnected mid-attempt; do not fail over.
			return
		}

		reason := failoverResponseSummary(capture)
		cooldown := state.markFailed(target, reason, time.Now())

		nextTarget := ""
		if i+1 < len(targets) {
			nextTarget = targets[i+1]
		}
		if nextTarget != "" {
			log.Warnf("selector=%s: failover: target=%s failed (%s); retrying target=%s (unhealthy for %s)", selectorID, target, reason, nextTarget, cooldown)
		} else {
			log.Warnf("selector=%s: failover: target=%s failed (%s); no targets left, returning its response (unhealthy for %s)", selectorID, target, reason, cooldown)
		}
		lastCapture = capture
	}

	// Every target failed: surface the last target's real response.
	if lastCapture != nil {
		if lastCapture.status == 0 {
			swaputil.SendResponse(w, r, http.StatusBadGateway, fmt.Sprintf("failover selector %q: all targets failed without a response", selectorID))
			return
		}
		lastCapture.flushBuffered()
		return
	}

	swaputil.SendResponse(w, r, http.StatusServiceUnavailable, fmt.Sprintf("failover selector %q has no targets", selectorID))
}

// newFailoverAttempt builds a replayable request for one target: a deep copy
// of the client request with the model rewritten to target and a fresh body
// reader, so downstream state (URL, headers, parsed forms) cannot leak between
// attempts.
func newFailoverAttempt(r *http.Request, body []byte, selectorID, target string) (*http.Request, error) {
	attempt := r.Clone(r.Context())
	attempt.Form = nil
	attempt.PostForm = nil
	attempt.MultipartForm = nil
	if body != nil {
		attempt.Body = io.NopCloser(bytes.NewReader(body))
		attempt.ContentLength = int64(len(body))
		attempt.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	return swaputil.ReplaceRequestModel(attempt, selectorID, target)
}
