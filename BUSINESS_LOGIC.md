# Business Logic Integration for ICAPeg

This document describes the business logic integration added to ICAPeg, including tenant ID validation, URL matching, and SQS integration.

## Overview

The business logic module (`icapeg/business`) provides:
1. **Tenant ID Validation**: Validates tenant (from ICAP server env) and user identity from ICAP/HTTP headers
2. **URL Matching**: Matches request URLs against configured rules in the database
3. **SQS Integration**: Sends recording requests to AWS SQS for asynchronous processing

## Configuration

### Environment Variables

The following environment variables are required. See [ENV_SETUP.md](./ENV_SETUP.md) for detailed setup instructions.

**Quick Setup**: Copy `.env.example` to `.env` and fill in your values:

```bash
# Tenant ID (required; set on ICAP server - proxy does not send it)
TENANT_ID=98fe190c-ea20-4c50-9d2c-e9766495da5e

# Database connection (PostgreSQL)
DATABASE_URL=postgresql://user:password@host:port/database?sslmode=disable

# SQS Configuration (optional - business logic will work without SQS)
RECORDING_QUEUE_URL=https://sqs.region.amazonaws.com/account/queue-name
AWS_REGION=us-east-1
AWS_ACCESS_KEY_ID=your-access-key-id
AWS_SECRET_ACCESS_KEY=your-secret-access-key
```

### Database Schema

The business logic expects the following database tables:

1. **tenants** table:
   - `id` (UUID, primary key)

2. **tenant_users** table:
   - `id` (UUID, primary key)
   - `tenant_id` (UUID, foreign key to tenants)
   - `identity_id` (string, user identity)
   - `last_activity_at` (timestamp)
   - `last_seen_ip` (string)
   - `updated_at` (timestamp)

3. **ai_tool_endpoints** table:
   - `endpoint_id` (UUID, primary key)
   - `tool_id` (UUID, foreign key to AI tools)
   - `name` (string, rule name)
   - `domain` (string host, e.g. `api.example.com`)
   - `endpoint_path` (string URL path, e.g. `/v1/chat/completions`)
   - `action` (string: `record`, `redact`, `smart`, `blind_redact`)
   - `flow` (string: `both`, `egress`, `ingress`)
   - `content_paths` (JSONB; array of objects describing payload locations, e.g. `[{"type": "prompt", "path": "messages[*].content"}]`)
   - `is_active` (boolean)
   - `created_at` (timestamp)

4. **tenant_ai_tools** table:
   - `tool_id` (UUID, foreign key to AI tools)
   - `tenant_id` (UUID, foreign key to tenants)
   - `is_monitored` (boolean; when false, tenant opts out of monitoring for that tool)

URL matching uses `ai_tool_endpoints` and `tenant_ai_tools` (see **Caching** below).

### Caching

- **ai_tools / ai_tool_endpoints**: Loaded once into memory at startup and refreshed every 5 minutes. No per-request fetch of endpoints; matching is done against this cache (by `protocol://domain` and `endpoint_path`).
- **tenant_ai_tools**: Not cached. For each request that matches a cached endpoint, a single query checks `is_monitored` for that `(tenant_id, tool_id)`. Missing row means default: monitored.

## Flow diagram

```mermaid
flowchart TB
    subgraph ICAP["ICAP request"]
        A[ToICAPEGServe] --> B[RequestInitialization]
        B --> C{REQMOD?}
    end

    subgraph Business["Business logic"]
        C -->|Yes| D[IdentityExtractor]
        D --> E[Tenant from env; user, IP from headers]
        E --> F[TenantValidator]
        F --> G{Valid tenant & user?}
        G -->|No| H[Return 400]
        G -->|Yes| I[URLMatcher]
        I --> J[Query ai_tool_endpoints + tenant_ai_tools]
        J --> K{Match found?}
        K -->|No| L[Return 204 pass-through]
        K -->|Yes| M{action?}
        M -->|record| N[processRecordAction in background]
        N --> O[Enqueue to SQS]
        N --> L
        M -->|redact / smart / blind_redact| P[Log; return 204]
        M -->|other flow| L
    end

    C -->|No| Q[Normal ICAP processing]
    H --> Q
    L --> Q
    P --> Q
    O --> Q
```

```mermaid
flowchart LR
    subgraph Input
        REQ[ICAP REQMOD]
    end

    subgraph Handler["BusinessLogicHandler"]
        ID[IdentityExtractor]
        TV[TenantValidator]
        UM[URLMatcher]
        SQS[SQSClient]
    end

    subgraph DB[(PostgreSQL)]
        T[tenants]
        TU[tenant_users]
        ATE[ai_tool_endpoints]
        TAT[tenant_ai_tools]
    end

    subgraph Output
        AWS[SQS Queue]
    end

    REQ --> ID
    ID --> TV
    TV --> T
    TV --> TU
    TV --> UM
    UM --> ATE
    UM --> TAT
    UM --> SQS
    SQS --> AWS
```

## Architecture

### Components

1. **IdentityExtractor** (`business/identity.go`):
   - Tenant ID: read from `TENANT_ID` environment variable on the ICAP server (not sent by proxy)
   - User ID and source IP: from ICAP/HTTP headers (JWT `Authorization`, G3 Proxy `X-Client-Username`, `X-Authenticated-User`, etc.)
     - `X-Forwarded-For` or `X-Real-IP` for source IP

2. **TenantValidator** (`business/tenant.go`):
   - Validates tenant existence in database
   - Validates user existence (non-blocking)
   - Updates user activity timestamps

3. **URLMatcher** (`business/urlmatcher.go`):
   - **Caches** all active `ai_tool_endpoints` (and their `tool_id` from `ai_tools`) in memory; refreshed on startup and every 5 minutes. No per-request fetch of endpoints.
   - Matches request base URL (tries both http and https) and path against the cached endpoints (exact path for `record`, prefix for `redact`/`smart`/`blind_redact`).
   - **Per request**: after a cache hit, performs a single DB query to `tenant_ai_tools` for `(tenant_id, tool_id)` to check `is_monitored`. No row means default: monitored.
   - Only cached endpoints have `flow` in (`egress`, `both`). Tenant opt-out (`is_monitored = false`) is applied via the per-request lookup.

4. **SQSClient** (`business/sqs.go`):
   - Sends recording requests to AWS SQS
   - Handles message size limits (1024KB max)
   - Includes message attributes for filtering/routing

5. **BusinessLogicHandler** (`business/handler.go`):
   - Orchestrates all business logic components
   - Processes requests based on URL configuration
   - Handles 'record' action (sends to SQS)
   - Handles 'redact' actions (placeholder for future implementation)

## Request Flow

1. ICAP request arrives at `ToICAPEGServe`
2. Request initialization (`RequestInitialization`)
3. For REQMOD requests, business logic is applied:
   - Extract identity from headers
   - Validate tenant and user
   - Match URL configuration
   - Process based on action:
     - **record**: Send to SQS (background), return 204
     - **redact/smart/blind_redact**: Process body (future), return 200 or 204
     - **no match**: Return 204 (pass through)
4. Continue with normal ICAP service processing

## Usage

### Initialization

The business logic handler is automatically initialized when the server starts (`server.StartServer`). It reads configuration from environment variables.

### Manual Initialization

```go
import "icapeg/business"

err := business.Init(
    databaseURL,
    sqsQueueURL,
    sqsRegion,
    sqsAccessKeyID,
    sqsSecretAccessKey,
)
if err != nil {
    // Handle error
}

handler := business.GetHandler()
```

### Processing a Request

The business logic is automatically applied during ICAP request processing. To manually process a request:

```go
handler := business.GetHandler()
statusCode, httpMsg, err := handler.ProcessRequest(
    icapHeaders,
    httpRequest,
    methodName,
    xICAPMetadata,
)
```

## Error Handling

- **Tenant validation failures**: Returns `BadRequestStatusCodeStr` (400)
- **Database errors**: Logged as warnings, request continues
- **SQS errors**: Logged as errors, request continues (non-blocking)
- **URL matching failures**: Returns `NoModificationStatusCodeStr` (204, pass through)

## SQS Message Format

Recording requests sent to SQS have the following format:

```json
{
  "body": "request body content",
  "headers": {
    "Content-Type": "application/json",
    ...
  },
  "url": "https://example.com/api/endpoint",
  "method": "POST",
  "user_id": "user-123",
  "tenant_id": "tenant-456",
  "region_code": "us-east",
  "session_id": "session-789",
  "source_ip": "192.168.1.1",
  "timestamp": "2024-01-15T10:30:00.000Z"
}
```

- `region_code`: Optional. Read from `REGION_CODE` env on the ICAP server. Backend resolves to `region_id` and stores it on the request record and user for region-based segregation.

Message attributes:
- `tenant_id`: For multi-tenant filtering
- `timestamp`: For time-based filtering
- `url`: For URL-based filtering (truncated to 256 chars)

## Dependencies

The business logic module requires:

- `github.com/lib/pq` - PostgreSQL driver
- `github.com/aws/aws-sdk-go` - AWS SDK for SQS
- `github.com/golang-jwt/jwt/v5` - JWT parsing

Add to `go.mod`:

```bash
go get github.com/lib/pq
go get github.com/aws/aws-sdk-go/aws
go get github.com/aws/aws-sdk-go/aws/session
go get github.com/aws/aws-sdk-go/service/sqs
go get github.com/golang-jwt/jwt/v5
```

## Testing

To test the business logic:

1. Set up PostgreSQL database with required tables
2. Configure environment variables
3. Start ICAPeg server
4. Set `TENANT_ID` on the ICAP server (e.g. in `.env`); the proxy does not send tenant ID. Send user/identity via `Authorization: Bearer <jwt-token>` or G3 Proxy headers as needed.

## Future Enhancements

- [ ] Implement redaction logic for 'redact' actions
- [x] Cache `ai_tool_endpoints`; per-request `tenant_ai_tools` lookup only
- [ ] Add metrics/monitoring
- [ ] Support for response modification (RESPMOD)
- [ ] Session ID extraction from cookies
