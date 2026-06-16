package business

import (
	"fmt"
	"os"
	"strconv"

	ldap "github.com/go-ldap/ldap/v3"
	"icapeg/logging"
)

// ADUserAttributes holds the AD attributes fetched for a proxy-authenticated user.
type ADUserAttributes struct {
	DisplayName string
	GivenName   string
	Email       string
	Department  string
}

// LDAPEnricher fetches user attributes from AD using a service-account bind.
// Configured entirely via environment variables:
//
//	LDAP_HOST          AD hostname or IP (e.g. localhost)
//	LDAP_PORT          LDAP port (default 389)
//	LDAP_BASE_DN       Search base (e.g. CN=Users,DC=gfoxai,DC=local)
//	LDAP_BIND_DN       Service-account bind DN
//	LDAP_BIND_PASSWORD Service-account password
type LDAPEnricher struct {
	host     string
	port     int
	baseDN   string
	bindDN   string
	bindPass string
}

func NewLDAPEnricher() *LDAPEnricher {
	port := 389
	if p := os.Getenv("LDAP_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	return &LDAPEnricher{
		host:     os.Getenv("LDAP_HOST"),
		port:     port,
		baseDN:   os.Getenv("LDAP_BASE_DN"),
		bindDN:   os.Getenv("LDAP_BIND_DN"),
		bindPass: os.Getenv("LDAP_BIND_PASSWORD"),
	}
}

// Enabled returns true when all required env vars are set.
func (e *LDAPEnricher) Enabled() bool {
	return e.host != "" && e.baseDN != "" && e.bindDN != ""
}

// FetchAttributes searches AD for the given username and returns display name, email, department.
// Returns (nil, nil) when enricher is disabled or user is not found — callers should treat this as non-fatal.
func (e *LDAPEnricher) FetchAttributes(username string) (*ADUserAttributes, error) {
	if !e.Enabled() || username == "" {
		return nil, nil
	}

	conn, err := ldap.DialURL(fmt.Sprintf("ldap://%s:%d", e.host, e.port))
	if err != nil {
		return nil, fmt.Errorf("LDAP dial: %w", err)
	}
	defer conn.Close()

	if err := conn.Bind(e.bindDN, e.bindPass); err != nil {
		return nil, fmt.Errorf("LDAP bind: %w", err)
	}

	// Match by sAMAccountName (Windows login name) or cn (common name).
	filter := fmt.Sprintf("(|(sAMAccountName=%s)(cn=%s))",
		ldap.EscapeFilter(username), ldap.EscapeFilter(username))

	req := ldap.NewSearchRequest(
		e.baseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		1, 0, false,
		filter,
		[]string{"displayName", "givenName", "mail", "department"},
		nil,
	)

	result, err := conn.Search(req)
	if err != nil {
		return nil, fmt.Errorf("LDAP search: %w", err)
	}
	if len(result.Entries) == 0 {
		logging.Logger.Warn(fmt.Sprintf("LDAP ENRICHER: no AD entry found for username=%q", username))
		return nil, nil
	}

	entry := result.Entries[0]
	attrs := &ADUserAttributes{
		DisplayName: entry.GetAttributeValue("displayName"),
		GivenName:   entry.GetAttributeValue("givenName"),
		Email:       entry.GetAttributeValue("mail"),
		Department:  entry.GetAttributeValue("department"),
	}

	if attrs.DisplayName == "" && attrs.GivenName != "" {
		attrs.DisplayName = attrs.GivenName
	}

	logging.Logger.Info(fmt.Sprintf(
		"LDAP ENRICHED: username=%s displayName=%q email=%q department=%q",
		username, attrs.DisplayName, attrs.Email, attrs.Department,
	))

	return attrs, nil
}
