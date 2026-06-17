# User Attribute Enrichment

AD/LDAP enrichment is owned by **ICAPeg**, not G3 proxy. G3 performs authentication-only (UPN bind) and passes the authenticated username via `X-Client-Username`. ICAPeg then does a service-account LDAP search to resolve the user's email, display name, and department before forwarding to URAI.

## Architecture

```
Browser → G3 Proxy (UPN bind: auth only)
               │
               │  ICAP REQMOD headers
               │  X-Client-Username: rohan.rn
               │  X-Client-IP:       172.18.0.1
               ▼
           ICAPeg (service-account LDAP search → normalize attrs)
               │
               │  SQS payload: user_attributes: {"email": "...", "display_name": "...", "department": "..."}
               ▼
           URAI backend (consumes normalized keys verbatim)
```

## G3 configuration

G3 binds using the UPN format (`username@domain`) — no `extra_ldap_attrs` needed:

```yaml
user_group:
  - name: default_users
    type: ldap
    ldap_url: "ldap://172.17.0.1:389/CN=Users,DC=gfoxai,DC=local"  # Linux: use Docker bridge IP; Mac/Windows: host.docker.internal
    username_attribute: cn
    user_principal_suffix: gfoxai.local   # bind as username@gfoxai.local
    response_timeout: 10s
    pool:
      min_idle_count: 1
    unmanaged_user:
      name: ad_user
      audit:
        enable_protocol_inspection: true
```

G3 emits only `X-Client-Username` and `X-Client-IP`/`X-Client-Port` to ICAPeg. No AD attributes are fetched or forwarded by G3.

## ICAPeg configuration (env vars)

Set the following environment variables in `icapeg/.env`:

| Variable | Example | Description |
|---|---|---|
| `LDAP_HOST` | `172.17.0.1` (Linux Docker bridge) or `host.docker.internal` (Mac/Windows Docker Desktop) | AD DC hostname or IP |
| `LDAP_PORT` | `389` | LDAP port (default: 389) |
| `LDAP_BASE_DN` | `CN=Users,DC=gfoxai,DC=local` | Search base |
| `LDAP_BIND_DN` | `CN=svc-icap,CN=Users,DC=gfoxai,DC=local` | Service account DN |
| `LDAP_BIND_PASSWORD` | `...` | Service account password |

If `LDAP_HOST`, `LDAP_BIND_DN`, or `LDAP_BIND_PASSWORD` are not set, ICAPeg logs a warning and runs without enrichment — requests are still processed but `user_attributes` will be empty.

## ICAPeg behaviour

On each request, ICAPeg calls `LDAPEnricher.Enrich(username)` which:

1. Strips the UPN suffix if present (`rohan.rn@gfoxai.local` → `rohan.rn`)
2. Checks a local in-memory TTL cache (5-minute expiry)
3. On cache miss: binds as the service account and searches `(sAMAccountName=<username>)`
4. Maps AD attributes to normalized contract keys:

| AD attribute | `user_attributes` key |
|---|---|
| `mail` | `email` |
| `displayName` | `display_name` |
| `department` | `department` |

The normalized map is forwarded in the SQS `RecordingRequest` as `user_attributes`.

## URAI backend

The backend reads `user_attributes` from the SQS payload and consumes the normalized keys directly:

- `email` → `tenant_users.email`
- `display_name` → `tenant_users.display_name`
- `department` → `tenant_users.department`
- Full map → merged into `tenant_users.user_metadata` JSONB

These fields are refreshed on every request (not write-once), so AD changes propagate automatically.
