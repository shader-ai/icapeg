package business

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"icapeg/logging"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// WhoisResult is the identity resolved for a WireGuard inner IP.
type WhoisResult struct {
	User     string `json:"user"`
	Subject  string `json:"subject"`
	Role     string `json:"role"`
	TenantID string `json:"tenant_id"`
}

type whoisEntry struct {
	result *WhoisResult
	expiry time.Time
}

var (
	whoisCacheMu sync.Mutex
	whoisCache   = make(map[string]*whoisEntry)
)

func lookupWhois(ctx context.Context, innerIP string, ts time.Time) (*WhoisResult, error) {
	backendURL := strings.TrimRight(os.Getenv("URAI_BACKEND_URL"), "/")
	whoisToken := os.Getenv("URAI_WHOIS_TOKEN")
	if backendURL == "" || whoisToken == "" {
		return nil, fmt.Errorf("identity: URAI_BACKEND_URL or URAI_WHOIS_TOKEN not configured")
	}

	bucket := ts.Unix() / 5
	cacheKey := fmt.Sprintf("%s:%d", innerIP, bucket)

	whoisCacheMu.Lock()
	if entry, ok := whoisCache[cacheKey]; ok && time.Now().Before(entry.expiry) {
		whoisCacheMu.Unlock()
		return entry.result, nil
	}
	whoisCacheMu.Unlock()

	tsStr := ts.UTC().Format(time.RFC3339)
	reqURL := fmt.Sprintf("%s/api/v1/tunnel/whois?ip=%s&ts=%s",
		backendURL, url.QueryEscape(innerIP), url.QueryEscape(tsStr))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("identity: whois request build: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+whoisToken)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity: whois request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// No active lease — cache the miss too
		whoisCacheMu.Lock()
		whoisCache[cacheKey] = &whoisEntry{result: nil, expiry: time.Now().Add(5 * time.Second)}
		whoisCacheMu.Unlock()
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("identity: whois returned HTTP %d", resp.StatusCode)
	}

	var result WhoisResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("identity: whois decode: %w", err)
	}

	whoisCacheMu.Lock()
	whoisCache[cacheKey] = &whoisEntry{result: &result, expiry: time.Now().Add(5 * time.Second)}
	whoisCacheMu.Unlock()
	return &result, nil
}

// IdentityExtractor extracts tenant ID, user ID, and source IP from headers and environment.
// Tenant ID is read from the TENANT_ID environment variable (e.g. set in .env or deployment).
type IdentityExtractor struct{}

// NewIdentityExtractor creates a new identity extractor
func NewIdentityExtractor() *IdentityExtractor {
	return &IdentityExtractor{}
}

// IdentityInfo contains extracted identity information
type IdentityInfo struct {
	TenantID string
	UserID   string // proxy username (primary) or JWT sub fallback for direct API calls
	Username string // friendly display name (e.g. X-Client-Username from G3 proxy)
	SourceIP string
}

// ExtractIdentity extracts tenant ID from env, user ID and source IP from ICAP/HTTP headers.
func (ie *IdentityExtractor) ExtractIdentity(icapHeaders, httpHeaders http.Header) (*IdentityInfo, error) {
	info := &IdentityInfo{}

	// Tenant ID is configured on the ICAP server via environment (e.g. .env or TENANT_ID).
	info.TenantID = strings.TrimSpace(os.Getenv("TENANT_ID"))

	// Strip client-supplied identity headers; only trust them from the proxy (icapHeaders).
	httpHeaders.Del("X-Client-Username")
	httpHeaders.Del("X-Authenticated-User")
	httpHeaders.Del("X-Client-IP")

	// Merge ICAP headers into HTTP headers (ICAP headers take priority)
	mergedHeaders := make(http.Header)
	for k, v := range httpHeaders {
		mergedHeaders[k] = v
	}
	for k, v := range icapHeaders {
		mergedHeaders[k] = v
	}

	// Resolve identity from WireGuard tunnel inner IP (takes priority over proxy-auth headers).
	if clientIP := mergedHeaders.Get("X-Client-IP"); clientIP != "" {
		whoisCtx, whoisCancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer whoisCancel()
		whoisResult, err := lookupWhois(whoisCtx, clientIP, time.Now())
		if err != nil {
			// When the tunnel path is enforced, a whois error must block the request —
			// no identity = no attribution, which violates the network-enforced guarantee.
			if os.Getenv("URAI_TUNNEL_ENFORCED") == "true" {
				return nil, fmt.Errorf("identity: whois lookup failed for %s on enforced tunnel path: %w", clientIP, err)
			}
			logging.Logger.Warn(fmt.Sprintf("whois lookup failed for IP %s: %v — falling back to header identity", clientIP, err))
			// Fall through to legacy header resolution on transient error (non-enforced path).
		} else if whoisResult == nil {
			// Hard-block: no active lease for this inner IP.
			return nil, fmt.Errorf("identity: no active lease for inner IP %s — request blocked", clientIP)
		} else {
			info.UserID = whoisResult.User
			info.Username = whoisResult.User
			info.SourceIP = clientIP
			if whoisResult.TenantID != "" {
				info.TenantID = whoisResult.TenantID
			}
			return info, nil
		}
	}

	// G3 proxy authentication headers take priority — the proxy username is the
	// authoritative organizational identity for ICAP-intercepted requests.
	if user := mergedHeaders.Get("X-Client-Username"); user != "" {
		info.UserID = user
		info.Username = user
	}

	if authUser := mergedHeaders.Get("X-Authenticated-User"); authUser != "" {
		// Decode Base64 "Local://{user}" format
		decoded, err := base64.StdEncoding.DecodeString(authUser)
		if err == nil {
			decodedStr := string(decoded)
			if userID, ok := strings.CutPrefix(decodedStr, "Local://"); ok {
				if info.UserID == "" {
					info.UserID = userID
				}
			}
		}
	}

	// JWT Bearer sub is a fallback only — used when no proxy username is available
	// (e.g. direct API calls that bypass the proxy).
	if auth := mergedHeaders.Get("Authorization"); auth != "" {
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) == 2 && strings.ToLower(parts[0]) == "bearer" {
			token := parts[1]
			parser := jwt.NewParser()
			claims := jwt.MapClaims{}
			_, _, err := parser.ParseUnverified(token, claims)
			if err == nil && info.UserID == "" {
				if sub, ok := claims["sub"].(string); ok {
					info.UserID = sub
				}
			}
		}
	}

	// Extract source IP
	if xForwardedFor := mergedHeaders.Get("X-Forwarded-For"); xForwardedFor != "" {
		// Take the first IP (original client)
		ips := strings.Split(xForwardedFor, ",")
		if len(ips) > 0 {
			info.SourceIP = strings.TrimSpace(ips[0])
		}
	} else if realIP := mergedHeaders.Get("X-Real-IP"); realIP != "" {
		info.SourceIP = realIP
	}

	// Validate that tenant_id is set via environment
	if info.TenantID == "" {
		return nil, errors.New("tenant_id not set: set TENANT_ID in environment or .env")
	}

	// User ID is optional - log warning but don't fail
	if info.UserID == "" {
		logging.Logger.Warn("Could not extract user_id from headers - continuing with empty user_id")
	}

	return info, nil
}
