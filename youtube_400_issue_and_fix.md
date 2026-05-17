# YouTube `log_event` 400 Bad Request Issue

## Issue Description
When using the ICAPeg server with G3Proxy, POST requests sent from the browser to YouTube's telemetry endpoint (`https://www.youtube.com/youtubei/v1/log_event?alt=json`) were consistently failing with a **400 Bad Request** error returned by the YouTube upstream server.

Simultaneously, `videoplayback` GET requests were failing with **403 Forbidden**.

## Root Cause Analysis

### 1. The `videoplayback` 403 Forbidden Error
This error occurs because YouTube signs its media playback URLs tightly to the specific client IP address that originally requested the video manifest.
- When the client's browser requests the video manifest, YouTube sees IP `A`.
- If the proxy forwards the actual video chunk requests (`videoplayback`) using a different outbound IP `B` (or if the VPN/proxy setup causes an IP mismatch), YouTube's security mechanisms reject the request with a **403 Forbidden** because the IP in the signed URL does not match the IP making the request.
- **Resolution:** This is a networking/routing configuration issue, not a bug in the ICAP server itself. Ensuring the egress IP matches or bypassing the proxy for these specific signed URLs resolves this.

### 2. The `log_event` 400 Bad Request Error
The 400 error was caused by a severe **HTTP payload corruption bug** within the ICAPeg server's `ContentTypes` utility.

When the ICAP server intercepted the `log_event` POST request, the `echo` service attempted to process the body. Here is the exact chain of events that corrupted the data:

1. **Content-Type Trigger:** The ICAP server inspects the `Content-Type: application/json` header and routes the body to `ContentTypes.GetContentType()`.
2. **Silent Unmarshal Failure:** The function attempts to call `json.Unmarshal(body, &data)` to check if the JSON has a `Base64` key. However, YouTube sends these payloads compressed (`Content-Encoding: gzip`). The `Unmarshal` function silently fails because it cannot parse gzipped binary data as a plain text string.
3. **Destructive Marshaling:** Despite the failure, the code proceeds to re-marshal the now-empty `data` map back into bytes using `json.Marshal(data)`. Marshaling an empty, nil map in Go results in the exact 4-byte string: `"null"`.
4. **Base64 Regex Collision:** The code then creates a `RegularFile` struct and passes the 4-byte `"null"` payload to `GetFileFromRequest()`. This function runs a strict regex to check if the payload is Base64 encoded. By sheer coincidence, the string `"null"` is valid Base64 (`n`, `u`, `l`, `l`), so the regex succeeds!
5. **Irreversible Decoding:** It blindly decodes `"null"` into 3 garbage bytes.
6. **State Loss:** Because `GetFileFromRequest()` and `BodyAfterScanning()` were defined with value receivers (`func (r RegularFile)`) instead of pointer receivers (`func (r *RegularFile)`), the `r.Encoded = true` state flag was immediately lost. 
7. **Final Corruption:** Because the flag was lost, the 3 garbage bytes were never re-encoded back to `"null"` before the ICAP response was formulated.

**Result:** The ICAP server replaced a 5000+ byte compressed telemetry payload with 3 garbage bytes and forwarded it to YouTube. YouTube rightfully responded with a 400 Bad Request.

## Applied Fix
The fixes were heavily focused on preventing destructive modifications to opaque or compressed payloads.

### `service/services-utilities/ContentTypes/contentType.go`
Modified the `GetContentType` function to respect HTTP compression headers:
- Added `if req.Header.Get("Content-Encoding") != ""` to immediately skip unmarshaling and return the raw, unmodified `bytes.Buffer` if the payload is compressed.
- Removed the destructive `json.Marshal(data)` step completely. If `Unmarshal` fails or the `Base64` key is not found, the function now returns the *original* unadulterated `body` slice instead of trying to reconstruct it. 

### `service/services-utilities/ContentTypes/regularFile.go`
Fixed the state-loss bug by updating the method receivers:
- Changed `func (r RegularFile)` to `func (r *RegularFile)` for both `GetFileFromRequest` and `BodyAfterScanning`.
- Updated `NewRegularFile` to return a pointer `*RegularFile`.
- This ensures that if a payload *is* genuinely Base64 decoded, the `Encoded` flag persists across the struct's lifecycle, guaranteeing it will be properly re-encoded before being sent to the client or upstream server.
