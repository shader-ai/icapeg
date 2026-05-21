package business

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"time"

	utils "icapeg/consts"
	"icapeg/logging"
)

// BusinessLogicHandler handles all business logic for ICAP requests
type BusinessLogicHandler struct {
	tenantValidator   *TenantValidator
	identityExtractor *IdentityExtractor
	urlMatcher        *URLMatcher
	sqsClient         *SQSClient
}

// NewBusinessLogicHandler creates a new business logic handler
func NewBusinessLogicHandler(db *sql.DB, sqsQueueURL, sqsRegion, sqsAccessKeyID, sqsSecretAccessKey string) (*BusinessLogicHandler, error) {
	tenantValidator := NewTenantValidator(db)
	identityExtractor := NewIdentityExtractor()
	urlMatcher := NewURLMatcher(db)

	var sqsClient *SQSClient
	var err error
	if sqsQueueURL != "" {
		logging.Logger.Info(fmt.Sprintf("ICAP SQS INIT: Initializing SQS client with queue URL: %s, region: %s", sqsQueueURL, sqsRegion))
		sqsClient, err = NewSQSClient(sqsQueueURL, sqsRegion, sqsAccessKeyID, sqsSecretAccessKey)
		if err != nil {
			logging.Logger.Warn(fmt.Sprintf("ICAP SQS INIT FAILED: Failed to initialize SQS client: %v. SQS functionality will be disabled.", err))
		} else {
			logging.Logger.Info(fmt.Sprintf("ICAP SQS INIT SUCCESS: SQS client initialized successfully for queue: %s", sqsQueueURL))
		}
	} else {
		logging.Logger.Warn("ICAP SQS INIT SKIPPED: RECORDING_QUEUE_URL not configured - SQS functionality will be disabled")
	}

	return &BusinessLogicHandler{
		tenantValidator:   tenantValidator,
		identityExtractor: identityExtractor,
		urlMatcher:        urlMatcher,
		sqsClient:         sqsClient,
	}, nil
}

// RefreshEndpointCache reloads ai_tool_endpoints from the database. Call on startup or periodically.
func (blh *BusinessLogicHandler) RefreshEndpointCache() {
	if blh.urlMatcher != nil {
		blh.urlMatcher.RefreshEndpointsCache()
	}
}

// ProcessRequest processes an ICAP request with business logic.
//
// bodyPartial means the encapsulated HTTP body may be ICAP preview-only (RFC 3507 Preview);
// in that case record actions must not enqueue yet — RespAndReqMods will call again after
// 100 Continue with the full body. Returns deferRecordingUntilFullBody=true when recording
// was skipped for this reason.
func (blh *BusinessLogicHandler) ProcessRequest(
	icapHeaders textproto.MIMEHeader,
	httpRequest *http.Request,
	methodName string,
	xICAPMetadata string,
	bodyPartial bool,
) (statusCode int, deferRecordingUntilFullBody bool, err error) {
	// Extract identity from headers
	icapHeaderMap := make(http.Header)
	for k, v := range icapHeaders {
		icapHeaderMap[k] = v
	}

	httpHeaderMap := make(http.Header)
	if httpRequest != nil {
		httpHeaderMap = httpRequest.Header
	}

	identity, err := blh.identityExtractor.ExtractIdentity(icapHeaderMap, httpHeaderMap)
	if err != nil {
		logging.Logger.Warn(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("Failed to extract identity: %v", err)))
		if identity == nil {
			return utils.BadRequestStatusCodeStr, false, fmt.Errorf("failed to extract identity: %v", err)
		}
	}

	// Validate tenant and user
	isValid, err := blh.tenantValidator.ValidateTenantAndUser(identity.TenantID, identity.UserID)
	if !isValid {
		logging.Logger.Warn(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("Tenant/user validation failed: %v", err)))
		return utils.BadRequestStatusCodeStr, false, fmt.Errorf("validation failed: %v", err)
	}

	// Get request URL
	requestURL := ""
	if httpRequest != nil {
		requestURL = httpRequest.URL.String()
		if requestURL == "" {
			requestURL = httpRequest.RequestURI
		}
		// Construct full URL if relative
		if !strings.HasPrefix(requestURL, "http") {
			scheme := "https"
			if httpRequest.TLS == nil {
				scheme = "http"
			}
			host := httpRequest.Host
			if host == "" {
				host = httpHeaderMap.Get("Host")
			}
			requestURL = fmt.Sprintf("%s://%s%s", scheme, host, requestURL)
		}
	}

	httpMethod := ""
	if httpRequest != nil {
		httpMethod = httpRequest.Method
	}

	// Match URL configuration (path + HTTP method must match catalog row)
	urlConfig, err := blh.urlMatcher.MatchURL(requestURL, identity.TenantID, httpMethod)
	if err != nil {
		logging.Logger.Error(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("Error matching URL: %v", err)))
		// Continue processing even if URL matching fails
	}

	if urlConfig != nil {
		action := strings.ToLower(urlConfig.Action)
		flow := strings.ToLower(urlConfig.Flow)

		switch flow {
		case "both", "egress":
			// continue
		default:
			logging.Logger.Debug(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("Flow '%s' not applicable - passing through", flow)))
			return utils.NoModificationStatusCodeStr, false, nil
		}

		switch action {
		case "record":
			if bodyPartial {
				logging.Logger.Info(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf(
					"ICAP RECORD DEFERRED: URL '%s' — body may be preview-only; will enqueue SQS after full body is reassembled (100 Continue path)",
					requestURL)))
				return utils.NoModificationStatusCodeStr, true, nil
			}
			// Snapshot body bytes synchronously before launching goroutine.
			// requiredService.Processing() will later exhaust httpRequest.Body via
			// CopyingFileToTheBuffer; capturing here avoids the race where the goroutine
			// reads an empty body.
			var bodySnapshot []byte
			if httpRequest != nil && httpRequest.Body != nil {
				var snapErr error
				bodySnapshot, snapErr = io.ReadAll(httpRequest.Body)
				if snapErr != nil {
					logging.Logger.Warn(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("ICAP BODY SNAPSHOT ERROR: %v", snapErr)))
				}
				// Restore body so the downstream ICAP service can still read it.
				httpRequest.Body = io.NopCloser(bytes.NewBuffer(bodySnapshot))
			}
			logging.Logger.Info(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("ICAP RECORD ACTION: Starting background processing for URL '%s' (action=%s, flow=%s)", requestURL, action, flow)))
			go blh.processRecordAction(httpRequest, bodySnapshot, identity, urlConfig, xICAPMetadata)
			return utils.NoModificationStatusCodeStr, false, nil
		case "redact", "smart", "blind_redact":
			logging.Logger.Info(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("Redaction action '%s' detected for URL '%s'", action, requestURL)))
			return utils.NoModificationStatusCodeStr, false, nil
		}
	}

	return utils.NoModificationStatusCodeStr, false, nil
}

// processRecordAction processes a record action in the background.
// bodyBytes must be pre-read by the caller before launching this goroutine; the caller
// also restores httpRequest.Body so that the ICAP service can still consume it.
func (blh *BusinessLogicHandler) processRecordAction(
	httpRequest *http.Request,
	bodyBytes []byte,
	identity *IdentityInfo,
	urlConfig *URLConfig,
	xICAPMetadata string,
) {
	requestURL := "unknown"
	method := "UNKNOWN"
	if httpRequest != nil {
		requestURL = httpRequest.URL.String()
		method = httpRequest.Method
	}

	logging.Logger.Info(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("ICAP PROCESS RECORD ACTION: Starting SQS processing for URL '%s'", requestURL)))

	if blh.sqsClient == nil {
		logging.Logger.Warn(utils.PrepareLogMsg(xICAPMetadata, "ICAP SQS CLIENT NOT INITIALIZED: SQS client is nil - skipping recording. Check RECORDING_QUEUE_URL environment variable."))
		return
	}

	logging.Logger.Debug(utils.PrepareLogMsg(xICAPMetadata, "ICAP SQS CLIENT INITIALIZED: Proceeding with recording request"))
	logging.Logger.Debug(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("ICAP BODY READ: Using pre-read snapshot of %d bytes", len(bodyBytes))))

	logging.Logger.Debug(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf(
		"ICAP RECORD PAYLOAD: json_valid=%v body_bytes=%d content_paths_bytes=%d",
		json.Valid(bodyBytes), len(bodyBytes), len(urlConfig.ContentPaths))))

	headers := make(map[string]string)
	if httpRequest != nil {
		for k, v := range httpRequest.Header {
			if len(v) > 0 {
				headers[k] = v[0]
			}
		}
	}

	sessionID := "default-session"
	if httpRequest != nil {
		if cookie, err := httpRequest.Cookie("session_id"); err == nil {
			sessionID = cookie.Value
		}
	}

	// Region code from env (e.g. us-east, eu); backend resolves to region_id when storing
	regionCode := strings.TrimSpace(os.Getenv("REGION_CODE"))

	recordingReq := &RecordingRequest{
		Body:           string(bodyBytes),
		Headers:        headers,
		URL:            requestURL,
		Method:         method,
		UserID:         identity.UserID,
		TenantID:       identity.TenantID,
		RegionCode:     regionCode,
		SessionID:      sessionID,
		SourceIP:       identity.SourceIP,
		ToolID:         urlConfig.ToolID,
		IsToolSanctioned: urlConfig.IsToolSanctioned,
		EndpointID:     urlConfig.ID,
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
	}
	if len(urlConfig.ContentPaths) > 0 {
		recordingReq.ContentPaths = append(json.RawMessage(nil), urlConfig.ContentPaths...)
	}

	logging.Logger.Info(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("ICAP PREPARING SQS MESSAGE: URL='%s', Method='%s', TenantID='%s', UserID='%s', RegionCode='%s', ToolID='%s', BodySize=%d bytes",
		recordingReq.URL, recordingReq.Method, recordingReq.TenantID, recordingReq.UserID, recordingReq.RegionCode, recordingReq.ToolID, len(bodyBytes))))

	if err := blh.sqsClient.EnqueueRecordingRequest(recordingReq); err != nil {
		logging.Logger.Error(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("ICAP SQS ENQUEUE FAILED: Failed to enqueue recording request for URL '%s': %v", recordingReq.URL, err)))
	} else {
		logging.Logger.Info(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("ICAP SQS ENQUEUE SUCCESS: Successfully enqueued recording for URL '%s'", recordingReq.URL)))
	}
}
