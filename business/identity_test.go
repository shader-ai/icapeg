package business

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"icapeg/logging"
)

func TestMain(m *testing.M) {
	logging.InitializeLogger("error", false)
	os.Exit(m.Run())
}

// newWhoisServer returns a test server mimicking /api/v1/tunnel/whois.
// resolveTo: non-nil → 200 with that result; nil → 404.
func newWhoisServer(t *testing.T, resolveTo *WhoisResult) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tunnel/whois" {
			http.NotFound(w, r)
			return
		}
		if resolveTo == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resolveTo)
	}))
}

func setWhoisEnv(t *testing.T, backendURL, token string) {
	t.Helper()
	t.Setenv("URAI_BACKEND_URL", backendURL)
	t.Setenv("URAI_WHOIS_TOKEN", token)
}

func clearWhoisCache() {
	whoisCacheMu.Lock()
	whoisCache = make(map[string]*whoisEntry)
	whoisCacheMu.Unlock()
}

// makeICAPHeader creates an http.Header with canonical keys (as textproto parsing would produce).
func makeICAPHeader(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

func TestExtractIdentity_ClientIPWins(t *testing.T) {
	clearWhoisCache()
	result := &WhoisResult{User: "alice@example.com", Subject: "sub-alice", Role: "urai-client", TenantID: "tenant-1"}
	srv := newWhoisServer(t, result)
	defer srv.Close()
	setWhoisEnv(t, srv.URL, "tok")
	t.Setenv("TENANT_ID", "tenant-from-env")

	ie := NewIdentityExtractor()
	icapH := makeICAPHeader("X-Client-IP", "10.66.1.5")
	httpH := makeICAPHeader("X-Client-Username", "should-be-ignored", "X-Client-IP", "attacker-injected")

	info, err := ie.ExtractIdentity(icapH, httpH)
	if err != nil {
		t.Fatalf("ExtractIdentity: %v", err)
	}
	if info.UserID != "alice@example.com" {
		t.Errorf("UserID = %q; want alice@example.com", info.UserID)
	}
	// Tenant from whois result overrides env.
	if info.TenantID != "tenant-1" {
		t.Errorf("TenantID = %q; want tenant-1", info.TenantID)
	}
}

func TestExtractIdentity_ClientIPStrippedFromHTTP(t *testing.T) {
	clearWhoisCache()
	t.Setenv("URAI_BACKEND_URL", "")
	t.Setenv("URAI_WHOIS_TOKEN", "")
	t.Setenv("TENANT_ID", "tenant-2")

	ie := NewIdentityExtractor()
	// X-Client-IP only in HTTP headers (not ICAP) — must be stripped, not trusted.
	icapH := http.Header{}
	httpH := makeICAPHeader(
		"X-Client-IP", "10.66.1.99",
		"X-Client-Username", "evil-user",
	)

	info, err := ie.ExtractIdentity(icapH, httpH)
	// Without ICAP X-Client-IP, whois is not called. Stripped HTTP identity means
	// UserID is empty (warning logged). Tenant from env is used.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.TenantID != "tenant-2" {
		t.Errorf("TenantID = %q; want tenant-2", info.TenantID)
	}
	if info.UserID != "" {
		t.Errorf("UserID = %q; want empty (client-supplied stripped)", info.UserID)
	}
}

func TestExtractIdentity_NoLease_HardBlock(t *testing.T) {
	clearWhoisCache()
	// 404 → whoisResult == nil → hard-block regardless of URAI_TUNNEL_ENFORCED.
	srv := newWhoisServer(t, nil)
	defer srv.Close()
	setWhoisEnv(t, srv.URL, "tok")
	t.Setenv("TENANT_ID", "tenant-3")

	ie := NewIdentityExtractor()
	icapH := makeICAPHeader("X-Client-IP", "10.66.1.7")
	httpH := http.Header{}

	_, err := ie.ExtractIdentity(icapH, httpH)
	if err == nil {
		t.Fatal("expected error for no-lease IP, got nil")
	}
}

func TestExtractIdentity_CacheHit(t *testing.T) {
	clearWhoisCache()
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		json.NewEncoder(w).Encode(&WhoisResult{User: "cached@example.com", TenantID: "t"})
	}))
	defer srv.Close()
	setWhoisEnv(t, srv.URL, "tok")
	t.Setenv("TENANT_ID", "t")
	t.Setenv("URAI_TUNNEL_ENFORCED", "false")

	ie := NewIdentityExtractor()
	icapH := makeICAPHeader("X-Client-IP", "10.66.1.10")

	// First call.
	if _, err := ie.ExtractIdentity(icapH, http.Header{}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Second call with same IP — must hit cache (same 5-second bucket).
	if _, err := ie.ExtractIdentity(icapH, http.Header{}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 backend call (cache hit on second), got %d", callCount)
	}
}
