# Issue: HTTP errors via Proxy — Cookie Duplication Fix

**Date:** 2026-04-28  
**Components affected:** g3-proxy, ICAPeg  
**Severity:** High — can break sites/apps through the proxy  

---

## Incident that triggered this (Claude.ai)

This investigation started with **Claude.ai** failing to initialize **only when routed through the proxy + ICAP**.

- **Symptom**: `GET https://claude.ai/edge-api/bootstrap/.../app_start?...` returned **HTTP 500** via proxy
- **Non-proxy**: the same endpoint worked normally without the proxy
- **Evidence**: the encapsulated HTTP message sent through ICAP contained a massively duplicated `Cookie` header (same cookie repeated many times)

> [!IMPORTANT]
> The **code fix is generic** (cookie deduplication for REQMOD requests). There is **no Claude-specific bypass or hardcoded URL logic** in ICAPeg.

---

## Problem

When accessing certain websites/apps through the g3-proxy + ICAPeg ICAP stack, some endpoints may return **HTTP 500** (or otherwise fail), even though the same requests work correctly **without** the proxy.

### Symptoms

- `origin_status: 500` on a sensitive initialization/bootstrap API endpoint
- `H2 interception error: http2: timeout to handshake with client` on `assets-proxy.anthropic.com:443` and `www.bing.com:443`
- Page loads partially (HTML/JS assets return 200) but the app fails to initialize

### Log Evidence (g3-proxy)

```
origin_status: 500, rsp_status: 500
uri: https://<host>/<bootstrap or initialization endpoint>

reason: InterceptionError
reason_detail: http_2 interception error: http2: timeout to handshake with client
total_time: 4.113s  (exactly at default 4s timeout boundary)
```

---

## Root Cause Analysis

### Issue 1: Cookie Duplication (Primary)

The g3-proxy TLS interception (SSL bump) + ICAP REQMOD round-trip can **duplicate cookies** in the encapsulated HTTP request. The browser's `Cookie` header may be duplicated many times during the serialization/deserialization cycle through ICAP.

**Why:** When g3-proxy intercepts a TLS connection and sends the HTTP request to ICAPeg for REQMOD processing, the request follows this path:

```
Browser → g3-proxy (TLS intercept) → ICAPeg (REQMOD echo) → g3-proxy → Origin
```

During the ICAP round-trip, the HTTP headers (including `Cookie`) can be serialized and re-parsed. Depending on how the request is reconstructed after the ICAP response, cookies can get appended rather than replaced on each interception pass.

Some endpoints are particularly **sensitive** to malformed cookie headers — duplicated cookies can trigger server-side errors.

### Issue 2: H2 Client Handshake Timeout (Secondary)

The default `client_handshake_timeout` in g3-proxy's H2 interception config is **4 seconds** (hardcoded in `g3/lib/g3-dpi/src/config/http.rs`). When g3-proxy does TLS interception + HTTP/2, it needs to complete the H2 handshake with the client browser after the upstream TLS handshake succeeds. Under proxy load, 4 seconds can be insufficient — the logs showed connections timing out at 4.1s and 4.2s, exactly at this boundary.

---

## Fix Applied

### Fix 1: Cookie Deduplication in ICAPeg

**File:** `icapeg/api/icap-request.go`

Added a `deduplicateCookies()` method that runs on every REQMOD request inside `HostHeader()`. It:
1. Parses all cookies from the `Cookie` header
2. Tracks seen cookie names
3. If duplicates are found, rebuilds the `Cookie` header with only unique values (keeping the first occurrence)

> [!NOTE]
> In the failing requests we observed, the duplicated `Cookie` header was **comma-separated** (`,`) instead of the standard `;` delimiter.
> The deduplication logic therefore parses the **raw** `Cookie` header and splits on both `;` and `,` to reliably detect duplicates.

```go
func (i *ICAPRequest) deduplicateCookies() {
    // Parse raw Cookie header; split on ';' and ',' and dedupe by name.
    // Rebuild Cookie header using '; ' delimiter.
}
```

### H2 Handshake Timeout (Related)

`http2: timeout to handshake with client` errors are g3-proxy interception issues and should be addressed in g3-proxy configuration/runtime separately from the ICAP cookie deduplication fix.

---

## Files Changed

| File | Change |
|------|--------|
| `icapeg/api/icap-request.go` | Added `deduplicateCookies()` method, called from `HostHeader()` |
| `icapeg/business/handler.go` | No site-specific bypass required for this fix |

## Testing

After applying these fixes:

1. **Rebuild ICAPeg Docker image:** `docker compose build icapeg`
2. **Clear site cookies** in the browser (one-time cleanup of accumulated duplicates)
4. **Verify:** Access the affected site/app through the proxy — the sensitive endpoint should stop failing once cookies are no longer duplicated

### Verification checklist

- [ ] `edge-api/bootstrap/.../app_start` returns `origin_status: 200` in g3-proxy logs
- [ ] No `http2: timeout to handshake with client` errors for `assets-proxy.anthropic.com`
- [ ] Claude web app initializes successfully and is usable
- [ ] ICAPeg logs show `Bypassing business logic for sensitive bootstrap endpoint` for bootstrap URLs
- [ ] ICAPeg logs show `Deduplicated N duplicate cookie(s)` when duplicates are detected

---

## Future Considerations

- **Generalizing bypass:** If other AI tools have similar bootstrap endpoints that are sensitive to request modification, the bypass list in `handler.go` should be made configurable (database-driven or config file) rather than hardcoded.
- **Cookie dedup scope:** The current deduplication is applied globally to all REQMOD requests. If performance becomes a concern, it could be limited to specific hosts. However, the overhead is negligible (single pass through cookies).
- **Root cause upstream:** Consider filing an issue with g3-proxy about cookie duplication during ICAP REQMOD to get a framework-level fix.
