package business

import (
	"context"
	"database/sql"
	"fmt"
	"icapeg/logging"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// isPathPlaceholder returns true if segment is a placeholder like {organization_id} or {id}.
func isPathPlaceholder(segment string) bool {
	s := strings.TrimSpace(segment)
	return len(s) >= 3 && strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")
}

// CachedEndpoint holds ai_tool_endpoint data for in-memory matching (no tenant_ai_tools).
type CachedEndpoint struct {
	EndpointID   string
	ToolID       string
	Name         string
	Action       string
	Flow         string
	HTTPMethod   string // e.g. POST; * or empty = match any verb
	Paths        []string
	BaseURL      string
	ContentPaths []byte // JSON from ai_tool_endpoints.content_paths; nil if NULL
}

// methodMatches returns true if the live request verb matches the catalog row.
// Configured "*" or empty string matches any request method.
func methodMatches(configuredMethod, requestMethod string) bool {
	cfg := strings.ToUpper(strings.TrimSpace(configuredMethod))
	req := strings.ToUpper(strings.TrimSpace(requestMethod))
	if cfg == "" || cfg == "*" {
		return true
	}
	if req == "" {
		return false
	}
	return cfg == req
}

// URLMatcher handles URL configuration lookup and matching.
// Active ai_tool_endpoints are cached; tenant_ai_tools is queried per request for is_monitored.
type URLMatcher struct {
	db    *sql.DB
	mu    sync.RWMutex
	cache map[string][]CachedEndpoint // base_url -> endpoints, order: created_at DESC
}

// URLConfig contains URL configuration data (passed to processRecordAction and into SQS for the recording worker).
type URLConfig struct {
	Name           string
	Action         string
	Flow           string
	Paths          []string
	ID             string // endpoint_id
	ToolID         string // ai_tools.tool_id (UUID) so recording worker can associate request with tool
	IsToolApproved *bool  // tenant_ai_tools.is_approved at request time (nil if no row = default false)
	ContentPaths   []byte // JSON for prompt extraction in recording worker; nil if NULL
}

// NewURLMatcher creates a new URL matcher and loads the endpoint cache from the database.
func NewURLMatcher(db *sql.DB) *URLMatcher {
	um := &URLMatcher{db: db, cache: make(map[string][]CachedEndpoint)}
	um.RefreshEndpointsCache()
	return um
}

// RefreshEndpointsCache loads all active ai_tool_endpoints (egress/both) into memory.
// Call on startup and optionally on a schedule. Does not include tenant_ai_tools.
func (um *URLMatcher) RefreshEndpointsCache() {
	if um.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	query := `
		SELECT 
			ate.endpoint_id::text,
			ate.tool_id::text,
			ate.name,
			ate.action,
			ate.flow,
			ate.http_method,
			ate.endpoint_path,
			ate.domain,
			ate.protocol,
			ate.content_paths
		FROM ai_tool_endpoints ate
		WHERE ate.is_active = true
			AND ate.flow IN ('egress', 'both')
		ORDER BY ate.domain, ate.created_at DESC
	`
	rows, err := um.db.QueryContext(ctx, query)
	if err != nil {
		logging.Logger.Error(fmt.Sprintf("ICAP ENDPOINT CACHE: failed to load: %v", err))
		return
	}
	defer rows.Close()

	newCache := make(map[string][]CachedEndpoint)
	for rows.Next() {
		var epID, toolID, name, action, flow, httpMethod, endpointPath, domain, protocol string
		var contentPaths []byte
		if err := rows.Scan(&epID, &toolID, &name, &action, &flow, &httpMethod, &endpointPath, &domain, &protocol, &contentPaths); err != nil {
			logging.Logger.Warn(fmt.Sprintf("ICAP ENDPOINT CACHE: skip row: %v", err))
			continue
		}
		// Build URL-matching configuration from new schema: domain + endpoint_path.
		// BaseURL is protocol://domain, and Paths contains a single endpoint_path (if present).
		baseURL := fmt.Sprintf("%s://%s", strings.ToLower(strings.TrimSpace(protocol)), strings.ToLower(strings.TrimSpace(domain)))
		var paths []string
		if endpointPath != "" {
			paths = append(paths, endpointPath)
		}
		entry := CachedEndpoint{
			EndpointID:   epID,
			ToolID:       toolID,
			Name:         name,
			Action:       action,
			Flow:         flow,
			HTTPMethod:   httpMethod,
			Paths:        paths,
			BaseURL:      baseURL,
			ContentPaths: append([]byte(nil), contentPaths...),
		}
		newCache[baseURL] = append(newCache[baseURL], entry)
	}
	um.mu.Lock()
	um.cache = newCache
	um.mu.Unlock()
	total := 0
	for _, list := range newCache {
		total += len(list)
	}
	logging.Logger.Info(fmt.Sprintf("ICAP ENDPOINT CACHE: loaded %d endpoints for %d base URLs", total, len(newCache)))
}

// tenantToolFlags returns is_monitored and is_approved for (tenant_id, tool_id).
// No row in tenant_ai_tools means default: monitored=true, approved=false.
func (um *URLMatcher) tenantToolFlags(ctx context.Context, tenantID, toolID string) (isMonitored bool, isApproved *bool, err error) {
	var monitored, approved bool
	rowErr := um.db.QueryRowContext(ctx,
		`SELECT is_monitored, is_approved FROM tenant_ai_tools WHERE tenant_id = $1 AND tool_id = $2`,
		tenantID, toolID,
	).Scan(&monitored, &approved)
	if rowErr == sql.ErrNoRows {
		return true, nil, nil // default: monitor=true, approved=nil (record as "unknown/default" or false per backend default)
	}
	if rowErr != nil {
		return false, nil, rowErr
	}
	return monitored, &approved, nil
}

// pathSegmentMatch returns true if requestPath matches configuredPath.
// configuredPath may contain placeholders like {organization_id} or {conversation_id},
// which match exactly one path segment (e.g. a UUID) in the request.
func pathSegmentMatch(requestPath, configuredPath string) bool {
	reqSegs := strings.Split(strings.Trim(requestPath, "/"), "/")
	cfgSegs := strings.Split(strings.Trim(configuredPath, "/"), "/")
	if len(reqSegs) != len(cfgSegs) {
		return false
	}
	for i := range cfgSegs {
		if isPathPlaceholder(cfgSegs[i]) {
			if reqSegs[i] == "" {
				return false
			}
			continue
		}
		if reqSegs[i] != cfgSegs[i] {
			return false
		}
	}
	return true
}

// isRegexPattern returns true if path is a regex pattern (starts with ^ or contains regex metacharacters).
func isRegexPattern(path string) bool {
	return strings.HasPrefix(path, "^") || strings.ContainsAny(path, "^$.*+?[](){}|")
}

// pathMatches returns true if requestPath matches configured paths for the given action.
// Supports: (1) regex patterns (if path starts with ^ or contains regex metacharacters),
//
//	(2) placeholder patterns like /api/org/{id}/completion,
//	(3) literal paths, (4) prefix matching for non-record action.
func pathMatches(requestPath string, paths []string, action string) bool {
	if len(paths) == 0 {
		return true
	}
	normReq := requestPath
	if !strings.HasPrefix(normReq, "/") {
		normReq = "/" + normReq
	}
	normReq = strings.TrimSuffix(normReq, "/") + "/"
	for _, configuredPath := range paths {
		normPath := configuredPath
		if !strings.HasPrefix(normPath, "/") {
			normPath = "/" + normPath
		}
		normPath = strings.TrimSuffix(normPath, "/") + "/"

		// Try regex match first (if path looks like a regex pattern)
		if isRegexPattern(configuredPath) {
			re, err := regexp.Compile(configuredPath)
			if err != nil {
				logging.Logger.Warn(fmt.Sprintf("Invalid regex pattern '%s': %v", configuredPath, err))
				continue
			}
			// Require full-string match: MatchString allows a prefix match when the pattern
			// omits trailing $ (e.g. ^/backend-anon/f/conversation/? matches .../prepare).
			for _, s := range []string{normReq, strings.TrimSuffix(normReq, "/")} {
				if s == "" {
					continue
				}
				if idx := re.FindStringIndex(s); idx != nil && idx[0] == 0 && idx[1] == len(s) {
					return true
				}
			}
			continue
		}

		// Try placeholder-aware segment match (for patterns like /api/organizations/{organization_id}/...)
		if pathSegmentMatch(normReq, normPath) {
			return true
		}

		// Fall back to exact or prefix matching
		if action == "record" {
			if normReq == normPath {
				return true
			}
		} else {
			if strings.HasPrefix(normReq, normPath) {
				return true
			}
		}
	}
	return false
}

// MatchURL looks up URL configuration using cached ai_tool_endpoints, then checks tenant_ai_tools for is_monitored.
// httpMethod is the encapsulated HTTP request method (e.g. POST); must match ai_tool_endpoints.http_method when set.
func (um *URLMatcher) MatchURL(requestURL, tenantID, httpMethod string) (*URLConfig, error) {
	if tenantID == "" {
		logging.Logger.Debug("URL matching skipped: tenant ID is empty")
		return nil, nil
	}

	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		logging.Logger.Warn(fmt.Sprintf("Error parsing URL '%s': %v", requestURL, err))
		return nil, nil
	}

	baseURL := fmt.Sprintf("%s://%s", parsedURL.Scheme, parsedURL.Host)
	requestPath := parsedURL.Path
	if !strings.HasPrefix(requestPath, "/") {
		requestPath = "/" + requestPath
	}
	if !strings.HasSuffix(requestPath, "/") {
		requestPath = requestPath + "/"
	}

	logging.Logger.Info(fmt.Sprintf("ICAP URL MATCHING START: requestURL='%s', baseURL='%s', path='%s', method='%s', tenantID='%s'",
		requestURL, baseURL, requestPath, httpMethod, tenantID))

	var altBaseURL string
	if strings.HasPrefix(baseURL, "http://") {
		altBaseURL = strings.Replace(baseURL, "http://", "https://", 1)
	} else if strings.HasPrefix(baseURL, "https://") {
		altBaseURL = strings.Replace(baseURL, "https://", "http://", 1)
	}

	// Find candidate endpoints from cache (try baseURL then altBaseURL, first path match wins)
	um.mu.RLock()
	candidates := append([]CachedEndpoint(nil), um.cache[baseURL]...)
	if altBaseURL != "" && len(candidates) == 0 {
		candidates = append(candidates, um.cache[altBaseURL]...)
	}
	if len(candidates) == 0 {
		allBases := make([]string, 0, len(um.cache))
		for key := range um.cache {
			allBases = append(allBases, key)
		}
		for _, key := range allBases {
			if !strings.Contains(key, "*.") {
				continue
			}
			wild := strings.Replace(key, "*.", "", 1)
			if strings.HasSuffix(baseURL, wild) || (altBaseURL != "" && strings.HasSuffix(altBaseURL, wild)) {
				candidates = append(candidates, um.cache[key]...)
			}
		}
	}
	um.mu.RUnlock()

	var matched *CachedEndpoint
	for i := range candidates {
		ep := &candidates[i]
		if !methodMatches(ep.HTTPMethod, httpMethod) {
			continue
		}
		if pathMatches(requestPath, ep.Paths, ep.Action) {
			matched = ep
			break
		}
	}
	if matched == nil {
		logging.Logger.Info(fmt.Sprintf("ICAP NO URL CONFIG FOUND: baseURL='%s', requestURL='%s', tenant='%s' (no cached endpoint or path match)",
			baseURL, requestURL, tenantID))
		return nil, nil
	}

	// Single DB lookup: is this tenant monitoring this tool and get approval status for audit
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	monitored, isApproved, err := um.tenantToolFlags(ctx, tenantID, matched.ToolID)
	if err != nil {
		logging.Logger.Error(fmt.Sprintf("Error checking tenant_ai_tools: %v", err))
		return nil, err
	}
	if !monitored {
		logging.Logger.Info(fmt.Sprintf("ICAP TENANT OPT-OUT: tenant='%s' tool_id='%s' is_monitored=false", tenantID, matched.ToolID))
		return nil, nil
	}

	logging.Logger.Info(fmt.Sprintf("ICAP URL MATCH FOUND: '%s' -> rule='%s' (action=%s, flow=%s, tool_id=%s, is_approved=%v)", requestURL, matched.Name, matched.Action, matched.Flow, matched.ToolID, isApproved))
	return &URLConfig{
		Name:           matched.Name,
		Action:         matched.Action,
		Flow:           matched.Flow,
		Paths:          matched.Paths,
		ID:             matched.EndpointID,
		ToolID:         matched.ToolID,
		IsToolApproved: isApproved,
		ContentPaths:   append([]byte(nil), matched.ContentPaths...),
	}, nil
}
