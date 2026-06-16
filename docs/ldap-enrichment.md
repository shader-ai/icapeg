# LDAP AD Enrichment

ICAPeg fetches user attributes from Active Directory on every ICAP request and forwards them downstream via request headers and the SQS recording payload.

## How it works

```
Browser → G3 Proxy (LDAP bind auth) → ICAPeg → AD (LDAP lookup) → upstream AI tool
                                          ↓
                                     SQS payload
                                     (user attrs)
                                          ↓
                                     URAI backend
```

1. **G3 Proxy** authenticates the user via a live LDAP bind against the AD DC. The authenticated username is forwarded to ICAPeg in the `X-Client-Username` header.
2. **ICAPeg** extracts the username from `X-Client-Username` and calls `LDAPEnricher.FetchAttributes()`.
3. **LDAPEnricher** binds to AD using a service account and searches for the user by `sAMAccountName` or `cn`.
4. The fetched attributes (`displayName`, `givenName`, `mail`, `department`) are:
   - Injected as request headers (`X-Client-Name`, `X-Client-Email`, `X-Client-Department`)
   - Logged via `logging.Logger`
   - Included in the SQS `RecordingRequest` payload for permanent storage in the URAI backend

## LDAPEnricher

File: `business/ldap_enricher.go`

Configured entirely via environment variables:

| Variable | Description | Example |
|---|---|---|
| `LDAP_HOST` | AD hostname or IP | `localhost` |
| `LDAP_PORT` | LDAP port (default `389`) | `389` |
| `LDAP_BASE_DN` | Search base DN | `CN=Users,DC=gfoxai,DC=local` |
| `LDAP_BIND_DN` | Service account bind DN | `CN=Administrator,CN=Users,DC=gfoxai,DC=local` |
| `LDAP_BIND_PASSWORD` | Service account password | `Admin@2024!` |

If any required variable (`LDAP_HOST`, `LDAP_BASE_DN`, `LDAP_BIND_DN`) is unset, enrichment is silently skipped — all other flows continue normally.

## AD attributes fetched

| AD Attribute | Header forwarded | SQS field |
|---|---|---|
| `displayName` | `X-Client-Name` | `display_name` |
| `givenName` | `X-Client-Given-Name` | — |
| `mail` | `X-Client-Email` | `email` |
| `department` | `X-Client-Department` | `department` |

## RecordingRequest fields added

```go
DisplayName string `json:"display_name,omitempty"` // from AD: displayName
Email       string `json:"email,omitempty"`         // from AD: mail
Department  string `json:"department,omitempty"`    // from AD: department
```

## Identity flow

```
X-Client-Username: rohan.rn
        ↓
LDAPEnricher.FetchAttributes("rohan.rn")
        ↓
AD search: (|(sAMAccountName=rohan.rn)(cn=rohan.rn))
        ↓
{DisplayName: "Rohan RN", Email: "rohan.rn@gfoxai.local", Department: "Engineering"}
        ↓
Injected into request headers + SQS payload
```

## Example ICAPeg log output

```
AD IDENTITY | user=rohan.rn | name="Rohan RN" | email="rohan.rn@gfoxai.local" | department="Engineering"
```
