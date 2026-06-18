package business

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	utils "icapeg/consts"
	"icapeg/logging"
)

// BusinessLogicHandler handles all business logic for ICAP requests
type BusinessLogicHandler struct {
	tenantValidator    *TenantValidator
	identityExtractor  *IdentityExtractor
	urlMatcher         *URLMatcher
	sqsClient          *SQSClient
	s3Client           *s3.S3
	s3Bucket           string
	awsRegion          string
	awsAccessKeyID     string
	awsSecretAccessKey string
	ldapEnricher       *LDAPEnricher
}

// NewBusinessLogicHandler creates a new business logic handler
func NewBusinessLogicHandler(db *sql.DB, sqsQueueURL, sqsRegion, sqsAccessKeyID, sqsSecretAccessKey, s3Bucket string, enricher *LDAPEnricher) (*BusinessLogicHandler, error) {
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

	// Initialize S3 client for file uploads (reuses the same AWS credentials as SQS).
	var s3Client *s3.S3
	if s3Bucket != "" && sqsRegion != "" {
		cfg := aws.NewConfig().
			WithRegion(sqsRegion).
			WithCredentials(credentials.NewStaticCredentials(sqsAccessKeyID, sqsSecretAccessKey, ""))
		sess, sessErr := session.NewSession(cfg)
		if sessErr != nil {
			logging.Logger.Warn(fmt.Sprintf("ICAP S3 INIT FAILED: %v — file uploads will be skipped", sessErr))
		} else {
			s3Client = s3.New(sess)
			logging.Logger.Info(fmt.Sprintf("ICAP S3 INIT SUCCESS: bucket=%s region=%s", s3Bucket, sqsRegion))
		}
	} else {
		logging.Logger.Warn("ICAP S3 INIT SKIPPED: URAI_FILE_BUCKET not configured — file uploads will be skipped")
	}

	return &BusinessLogicHandler{
		tenantValidator:    tenantValidator,
		identityExtractor:  identityExtractor,
		urlMatcher:         urlMatcher,
		sqsClient:          sqsClient,
		s3Client:           s3Client,
		s3Bucket:           s3Bucket,
		awsRegion:          sqsRegion,
		awsAccessKeyID:     sqsAccessKeyID,
		awsSecretAccessKey: sqsSecretAccessKey,
		ldapEnricher:       enricher,
	}, nil
}

// randomHex returns n random bytes hex-encoded, for collision-free S3 object names.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Fall back to a timestamp-based value; extremely unlikely to be hit.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// uploadFilesToS3 extracts every file part from a multipart/form-data body and uploads each to
// S3, returning one RecordedFile per successfully-stored attachment. Returns nil if the body is
// not multipart or no file part is present. A single failed part is skipped (others still upload).
//
// Each object key uses a random UUID-style name (not the raw upload filename) to avoid
// collisions and path traversal: file-uploads/<tenant_id>/<record_id>/<uuid><ext>.
func (blh *BusinessLogicHandler) uploadFilesToS3(
	tenantID, recordID string,
	bodyBytes []byte,
	contentTypeHeader string,
	xICAPMetadata string,
) []RecordedFile {
	if blh.s3Client == nil || blh.s3Bucket == "" {
		return nil
	}

	mediaType, params, err := mime.ParseMediaType(contentTypeHeader)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return nil
	}

	boundary := params["boundary"]
	if boundary == "" {
		return nil
	}

	mr := multipart.NewReader(bytes.NewReader(bodyBytes), boundary)
	var files []RecordedFile
	for {
		part, partErr := mr.NextPart()
		if partErr != nil {
			break
		}
		fileBytes, readErr := io.ReadAll(part)
		part.Close()
		if readErr != nil || len(fileBytes) == 0 {
			continue
		}

		partFileName := part.FileName()
		if partFileName == "" {
			continue // skip non-file fields
		}

		partContentType := part.Header.Get("Content-Type")
		if partContentType == "" {
			partContentType = "application/octet-stream"
		}

		// Random object name + extension derived from the original filename.
		ext := strings.ToLower(filepath.Ext(partFileName))
		key := fmt.Sprintf("file-uploads/%s/%s/%s%s", tenantID, recordID, randomHex(16), ext)

		sum := sha256.Sum256(fileBytes)
		hexSum := hex.EncodeToString(sum[:])

		_, putErr := blh.s3Client.PutObject(&s3.PutObjectInput{
			Bucket:      aws.String(blh.s3Bucket),
			Key:         aws.String(key),
			Body:        bytes.NewReader(fileBytes),
			ContentType: aws.String(partContentType),
		})
		if putErr != nil {
			logging.Logger.Error(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf(
				"ICAP S3 UPLOAD FAILED: key=%s err=%v", key, putErr)))
			continue
		}

		logging.Logger.Info(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf(
			"ICAP S3 UPLOAD SUCCESS: key=%s size=%d sha256=%s", key, len(fileBytes), hexSum)))
		files = append(files, RecordedFile{
			FileS3Key:       key,
			FileName:        partFileName,
			FileContentType: partContentType,
			FileSize:        int64(len(fileBytes)),
			FileSHA256:      hexSum,
		})
	}

	return files
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

	// Enrich identity with AD attributes via service-account LDAP search.
	// G3 passes only X-Client-Username; ICAPeg owns enrichment (email, display_name, department).
	userAttributes := make(map[string]string)
	if blh.ldapEnricher != nil && identity.Username != "" {
		if attrs, enrichErr := blh.ldapEnricher.Enrich(identity.Username); enrichErr != nil {
			logging.Logger.Warn(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf(
				"LDAP enrich failed for %q: %v", identity.Username, enrichErr)))
		} else {
			userAttributes = attrs
		}
	}
	logging.Logger.Debug(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf(
		"CLIENT IDENTITY: username=%s attributes=%v", identity.UserID, userAttributes,
	)))

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
			go blh.processRecordAction(httpRequest, bodySnapshot, identity, userAttributes, urlConfig, xICAPMetadata)
			return utils.NoModificationStatusCodeStr, false, nil
		case "redact", "smart", "blind_redact":
			logging.Logger.Debug(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("redact action '%s' passthrough for url='%s'", action, requestURL)))
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
	userAttributes map[string]string,
	urlConfig *URLConfig,
	xICAPMetadata string,
) {
	requestURL := "unknown"
	method := "UNKNOWN"
	if httpRequest != nil {
		requestURL = httpRequest.URL.String()
		method = httpRequest.Method
	}

	if blh.sqsClient == nil {
		logging.Logger.Warn(utils.PrepareLogMsg(xICAPMetadata, "ICAP SQS CLIENT NOT INITIALIZED: SQS client is nil - skipping recording. Check RECORDING_QUEUE_URL environment variable."))
		return
	}

	logging.Logger.Debug(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("body snapshot: %d bytes", len(bodyBytes))))

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
		Body:             string(bodyBytes),
		Headers:          headers,
		URL:              requestURL,
		Method:           method,
		UserID:           identity.UserID,
		Username:         identity.Username,
		UserAttributes:   userAttributes,
		TenantID:         identity.TenantID,
		RegionCode:       regionCode,
		SessionID:        sessionID,
		SourceIP:         identity.SourceIP,
		ToolID:           urlConfig.ToolID,
		IsToolSanctioned: urlConfig.IsToolSanctioned,
		EndpointID:       urlConfig.ID,
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
	}
	if len(urlConfig.ContentPaths) > 0 {
		recordingReq.ContentPaths = append(json.RawMessage(nil), urlConfig.ContentPaths...)
	}

	// Detect multipart file upload: extract file, upload to S3, attach key to SQS message.
	// Use a deterministic record ID derived from the SQS MessageId (same logic as backend).
	contentTypeHdr := ""
	if httpRequest != nil {
		contentTypeHdr = httpRequest.Header.Get("Content-Type")
	}
	if files := blh.uploadFilesToS3(
		identity.TenantID,
		fmt.Sprintf("%d", time.Now().UnixNano()), // proxy record ID — backend derives its own from SQS MessageId
		bodyBytes,
		contentTypeHdr,
		xICAPMetadata,
	); len(files) > 0 {
		recordingReq.Files = files
		// Mirror the first attachment into the legacy single fields for backward compatibility.
		first := files[0]
		recordingReq.FileS3Key = first.FileS3Key
		recordingReq.FileName = first.FileName
		recordingReq.FileContentType = first.FileContentType
		recordingReq.FileSize = first.FileSize
		recordingReq.FileSHA256 = first.FileSHA256
		recordingReq.HasFileAttachment = true
		// Don't send the raw binary body for file uploads — it's already in S3.
		recordingReq.Body = ""
	}

	logging.Logger.Info(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("ICAP PREPARING SQS MESSAGE: URL='%s', Method='%s', TenantID='%s', UserID='%s', RegionCode='%s', ToolID='%s', BodySize=%d bytes",
		recordingReq.URL, recordingReq.Method, recordingReq.TenantID, recordingReq.UserID, recordingReq.RegionCode, recordingReq.ToolID, len(bodyBytes))))

	if err := blh.sqsClient.EnqueueRecordingRequest(recordingReq); err != nil {
		logging.Logger.Error(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("ICAP SQS ENQUEUE FAILED: Failed to enqueue recording request for URL '%s': %v", recordingReq.URL, err)))
	} else {
		logging.Logger.Info(utils.PrepareLogMsg(xICAPMetadata, fmt.Sprintf("ICAP SQS ENQUEUE SUCCESS: Successfully enqueued recording for URL '%s'", recordingReq.URL)))
	}
}
