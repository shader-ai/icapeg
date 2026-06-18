package business

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/go-ldap/ldap/v3"
)

const ldapEnricherCacheTTL = 5 * time.Minute

type ldapCacheEntry struct {
	attrs  map[string]string
	expiry time.Time
}

// LDAPEnricher does a service-account LDAP search for a given sAMAccountName
// and returns normalized attributes: email, display_name, department.
// Results are cached with a TTL to avoid per-request LDAP traffic.
type LDAPEnricher struct {
	host         string
	port         string
	baseDN       string
	bindDN       string
	bindPassword string

	mu    sync.Mutex
	cache map[string]ldapCacheEntry
}

func NewLDAPEnricher(host, port, baseDN, bindDN, bindPassword string) *LDAPEnricher {
	return &LDAPEnricher{
		host:         host,
		port:         port,
		baseDN:       baseDN,
		bindDN:       bindDN,
		bindPassword: bindPassword,
		cache:        make(map[string]ldapCacheEntry),
	}
}

// Enrich looks up AD attributes for the given sAMAccountName.
// Returns a map with keys: email, display_name, department (absent when not found in AD).
// A not-found result is also cached to avoid hammering AD for unknown users.
func (e *LDAPEnricher) Enrich(username string) (map[string]string, error) {
	if username == "" {
		return nil, nil
	}

	// Strip UPN suffix if present (e.g. user@domain → user)
	if idx := strings.Index(username, "@"); idx != -1 {
		username = username[:idx]
	}

	e.mu.Lock()
	if entry, ok := e.cache[username]; ok && time.Now().Before(entry.expiry) {
		result := make(map[string]string, len(entry.attrs))
		for k, v := range entry.attrs {
			result[k] = v
		}
		e.mu.Unlock()
		return result, nil
	}
	e.mu.Unlock()

	attrs, err := e.searchAD(username)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	e.cache[username] = ldapCacheEntry{
		attrs:  attrs,
		expiry: time.Now().Add(ldapEnricherCacheTTL),
	}
	e.mu.Unlock()

	return attrs, nil
}

func (e *LDAPEnricher) searchAD(username string) (map[string]string, error) {
	addr := fmt.Sprintf("%s:%s", e.host, e.port)
	conn, err := ldap.DialURL("ldap://"+addr, ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}))
	if err != nil {
		return nil, fmt.Errorf("ldap dial %s: %w", addr, err)
	}
	defer conn.Close()

	if err := conn.Bind(e.bindDN, e.bindPassword); err != nil {
		return nil, fmt.Errorf("ldap service-account bind: %w", err)
	}

	filter := fmt.Sprintf("(sAMAccountName=%s)", ldap.EscapeFilter(username))
	req := ldap.NewSearchRequest(
		e.baseDN,
		ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases,
		1, // size limit
		10, // time limit seconds
		false,
		filter,
		[]string{"mail", "displayName", "department"},
		nil,
	)

	res, err := conn.Search(req)
	if err != nil {
		return nil, fmt.Errorf("ldap search for %q: %w", username, err)
	}

	attrs := make(map[string]string)
	if len(res.Entries) == 0 {
		return attrs, nil
	}

	entry := res.Entries[0]
	if v := entry.GetAttributeValue("mail"); v != "" {
		attrs["email"] = v
	}
	if v := entry.GetAttributeValue("displayName"); v != "" {
		attrs["display_name"] = v
	}
	if v := entry.GetAttributeValue("department"); v != "" {
		attrs["department"] = v
	}

	return attrs, nil
}
