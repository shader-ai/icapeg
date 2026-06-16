package business

import (
	"encoding/base64"
	"errors"
	"icapeg/logging"
	"net/http"
	"os"
	"strings"

	"github.com/golang-jwt/jwt/v4"
)

// IdentityExtractor extracts tenant ID, user ID, and source IP from headers and environment.
// Tenant ID is read from the TENANT_ID environment variable (e.g. set in .env or deployment).
type IdentityExtractor struct{}

// NewIdentityExtractor creates a new identity extractor
func NewIdentityExtractor() *IdentityExtractor {
	return &IdentityExtractor{}
}

// IdentityInfo contains extracted identity information
type IdentityInfo struct {
	TenantID    string
	UserID      string // proxy username (primary) or JWT sub fallback for direct API calls
	Username    string // friendly display name (e.g. X-Client-Username from G3 proxy)
	SourceIP    string
	// AD attributes — populated by LDAPEnricher after identity extraction
	DisplayName string
	GivenName   string
	Email       string
	Department  string
}

// ExtractIdentity extracts tenant ID from env, user ID and source IP from ICAP/HTTP headers.
func (ie *IdentityExtractor) ExtractIdentity(icapHeaders, httpHeaders http.Header) (*IdentityInfo, error) {
	info := &IdentityInfo{}

	// Tenant ID is configured on the ICAP server via environment (e.g. .env or TENANT_ID).
	info.TenantID = strings.TrimSpace(os.Getenv("TENANT_ID"))

	// Merge ICAP headers into HTTP headers (ICAP headers take priority)
	mergedHeaders := make(http.Header)
	for k, v := range httpHeaders {
		mergedHeaders[k] = v
	}
	for k, v := range icapHeaders {
		mergedHeaders[k] = v
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
			if strings.HasPrefix(decodedStr, "Local://") {
				userID := strings.TrimPrefix(decodedStr, "Local://")
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
			_, err := parser.ParseUnverified(token, claims)
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
