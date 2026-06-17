# User Attribute Enrichment

AD/LDAP enrichment is owned by **G3 proxy**, not ICAPeg. ICAPeg is a governance/inspection layer — it reads whatever headers G3 injects and forwards them downstream. No LDAP credentials or connections are needed in ICAPeg.

## Architecture

```
Browser → G3 Proxy (live LDAP bind: auth + fetch attrs)
               │
               │  ICAP REQMOD headers
               │  X-Client-Username: rohan.rn
               │  X-Client-IP:       172.18.0.1
               │  X-Client-mail:     rohan.rn@gfoxai.com
               │  X-Client-givenName: Rohan
               │  X-Client-sn:       RN
               ▼
           ICAPeg (reads X-Client-* headers dynamically — no LDAP needed)
               │
               │  SQS payload: user_attributes: {"mail": "...", "givenname": "..."}
               ▼
           URAI backend (merges into user_metadata JSONB)
```

## G3 configuration

In `g3proxy.yaml`, set `extra_ldap_attrs` under the `ldap` user group:

```yaml
user_group:
  - name: default_users
    type: ldap
    ldap_url: "ldap://host.docker.internal:389/CN=Users,DC=gfoxai,DC=local"
    username_attribute: cn
    response_timeout: 10s
    extra_ldap_attrs:
      - mail
      - givenName
      - sn
      - department    # only if populated in your AD
```

G3 performs a live LDAP bind per connection for authentication, then (if `extra_ldap_attrs` is non-empty) fetches those attributes in the same session and injects them as `X-Client-<AttrName>` ICAP headers.

## ICAPeg behaviour

ICAPeg reads every `X-Client-*` header dynamically — no hardcoded field names. New AD attributes added to G3's `extra_ldap_attrs` flow through automatically without any ICAPeg code changes.

Headers are normalised to snake_case keys and forwarded in the SQS `RecordingRequest` as `user_attributes`:

| G3 ICAP header | `user_attributes` key |
|---|---|
| `X-Client-Username` | `username` |
| `X-Client-mail` | `mail` |
| `X-Client-givenName` | `givenname` |
| `X-Client-sn` | `sn` |
| `X-Client-department` | `department` |

## URAI backend

The backend reads `user_attributes` from the SQS payload and merges it into the user's `user_metadata` JSONB column. Well-known keys are promoted to dedicated columns (`mail` → `email`, `givenName`/`sn` → `display_name`, `department` → `department`).
