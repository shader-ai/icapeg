package business

import (
	"context"
	"database/sql"
	"fmt"
	"icapeg/logging"
	"os"
	"strings"
	"sync"
	"time"
)

// CandidateLogger accumulates unmatched-domain counters in memory and batches
// them to candidate_domains every flushInterval. Never does a synchronous DB
// write per request.
type CandidateLogger struct {
	db            *sql.DB
	mu            sync.Mutex
	counts        map[string]int64
	denylist      map[string]struct{}
	flushInterval time.Duration
	stopCh        chan struct{}
}

// NewCandidateLogger creates a logger and starts the background flush goroutine.
func NewCandidateLogger(db *sql.DB) *CandidateLogger {
	cl := &CandidateLogger{
		db:            db,
		counts:        make(map[string]int64),
		denylist:      buildDenylist(),
		flushInterval: 30 * time.Second,
		stopCh:        make(chan struct{}),
	}
	go cl.flushLoop()
	return cl
}

// Observe is the hot-path call. Pre-filters and increments an in-memory counter.
// No DB I/O; safe to call from any goroutine.
func (cl *CandidateLogger) Observe(domain, method, contentType string) {
	if !candidatePreFilter(method, contentType, domain, cl.denylist) {
		return
	}
	cl.mu.Lock()
	cl.counts[domain]++
	cl.mu.Unlock()
}

// Stop terminates the flush goroutine.
func (cl *CandidateLogger) Stop() {
	close(cl.stopCh)
}

func (cl *CandidateLogger) flushLoop() {
	ticker := time.NewTicker(cl.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			cl.flush()
		case <-cl.stopCh:
			cl.flush() // final flush on shutdown
			return
		}
	}
}

func (cl *CandidateLogger) flush() {
	cl.mu.Lock()
	if len(cl.counts) == 0 {
		cl.mu.Unlock()
		return
	}
	snapshot := cl.counts
	cl.counts = make(map[string]int64)
	cl.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const upsertSQL = `
		INSERT INTO candidate_domains (domain, request_count, first_seen, last_seen)
		VALUES ($1, $2, now(), now())
		ON CONFLICT (domain) DO UPDATE
		SET request_count = candidate_domains.request_count + EXCLUDED.request_count,
		    last_seen = now()
	`
	for domain, count := range snapshot {
		if _, err := cl.db.ExecContext(ctx, upsertSQL, domain, count); err != nil {
			logging.Logger.Error(fmt.Sprintf("CANDIDATE LOGGER FLUSH ERROR: domain=%s err=%v", domain, err))
		}
	}
	logging.Logger.Info(fmt.Sprintf("CANDIDATE LOGGER FLUSH: %d domains", len(snapshot)))
}

// candidatePreFilter returns true if the request is plausibly an AI API call
// worth tracking. Drops infra/asset/telemetry noise early.
func candidatePreFilter(method, contentType, domain string, denylist map[string]struct{}) bool {
	m := strings.ToUpper(strings.TrimSpace(method))
	if m != "POST" && m != "PUT" {
		return false
	}

	ct := strings.ToLower(contentType)
	// Keep only JSON-family and multipart (API payloads).
	if !strings.Contains(ct, "application/json") &&
		!strings.Contains(ct, "application/") && !strings.HasSuffix(ct, "+json") &&
		!strings.Contains(ct, "text/event-stream") &&
		!strings.Contains(ct, "multipart/form-data") {
		// Be permissive: if content-type is empty let it through (may be missing header)
		if ct != "" {
			// Explicit drop list
			if strings.HasPrefix(ct, "text/html") ||
				strings.HasPrefix(ct, "image/") ||
				strings.HasPrefix(ct, "application/javascript") ||
				strings.HasPrefix(ct, "font/") ||
				strings.HasPrefix(ct, "text/css") ||
				strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
				return false
			}
		}
	}

	d := strings.ToLower(domain)
	// Exact denylist match
	if _, ok := denylist[d]; ok {
		return false
	}
	// Suffix denylist match
	for suffix := range denylist {
		if strings.HasPrefix(suffix, ".") && strings.HasSuffix(d, suffix) {
			return false
		}
	}
	return true
}

// buildDenylist builds the infra/CDN/analytics domain denylist from the
// CANDIDATE_DENYLIST env var (comma-separated additions) plus built-in defaults.
func buildDenylist() map[string]struct{} {
	defaults := []string{
		"google-analytics.com",
		"doubleclick.net",
		"sentry.io",
		".cloudfront.net",
		"segment.io",
		"segment.com",
		"fonts.gstatic.com",
		"fonts.googleapis.com",
		"googletagmanager.com",
		"analytics.google.com",
		"hotjar.com",
		"intercom.io",
		"intercomcdn.com",
		"mixpanel.com",
		"amplitude.com",
		"fullstory.com",
		"newrelic.com",
		"datadog-browser-agent.com",
		"datadoghq.com",
		"rollbar.com",
		"bugsnag.com",
		"logrocket.com",
		"cdn.jsdelivr.net",
		"unpkg.com",
		"cdnjs.cloudflare.com",
		"ajax.googleapis.com",
		"static.cloudflareinsights.com",
	}

	m := make(map[string]struct{}, len(defaults)+16)
	for _, d := range defaults {
		m[strings.ToLower(d)] = struct{}{}
	}

	if extra := os.Getenv("CANDIDATE_DENYLIST"); extra != "" {
		for _, d := range strings.Split(extra, ",") {
			d = strings.ToLower(strings.TrimSpace(d))
			if d != "" {
				m[d] = struct{}{}
			}
		}
	}
	return m
}
