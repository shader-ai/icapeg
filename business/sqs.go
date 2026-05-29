package business

import (
	"context"
	"encoding/json"
	"fmt"
	"icapeg/logging"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sqs"
)

// SQSClient handles sending messages to SQS
type SQSClient struct {
	client   *sqs.SQS
	queueURL string
	region   string
}

// NewSQSClient creates a new SQS client
func NewSQSClient(queueURL, region, accessKeyID, secretAccessKey string) (*SQSClient, error) {
	if queueURL == "" {
		return nil, fmt.Errorf("SQS queue URL not configured")
	}

	// Create AWS session
	cfg := aws.NewConfig().
		WithRegion(region).
		WithCredentials(credentials.NewStaticCredentials(accessKeyID, secretAccessKey, ""))

	sess, err := session.NewSession(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create AWS session: %v", err)
	}

	return &SQSClient{
		client:   sqs.New(sess),
		queueURL: queueURL,
		region:   region,
	}, nil
}

// RecordingRequest represents a recording request message
type RecordingRequest struct {
	Body             string            `json:"body"`
	Headers          map[string]string `json:"headers"`
	URL              string            `json:"url"`
	Method           string            `json:"method"`
	UserID           string            `json:"user_id,omitempty"`
	Username         string            `json:"username,omitempty"` // friendly display name from proxy (e.g. X-Client-Username)
	TenantID         string            `json:"tenant_id,omitempty"`
	RegionCode       string            `json:"region_code,omitempty"` // Region code (e.g. us-east, eu) for this ICAP deployment; backend resolves to region_id
	SessionID        string            `json:"session_id,omitempty"`
	SourceIP         string            `json:"source_ip,omitempty"`
	ToolID           string            `json:"tool_id,omitempty"`
	IsToolSanctioned *bool             `json:"is_tool_sanctioned,omitempty"`
	EndpointID       string            `json:"endpoint_id,omitempty"`
	ContentPaths     json.RawMessage   `json:"content_paths,omitempty"`
	Timestamp        string            `json:"timestamp"`
	// File attachment fields — populated when the intercepted request is a multipart file upload.
	FileS3Key        string `json:"file_s3_key,omitempty"`
	FileName         string `json:"file_name,omitempty"`
	FileContentType  string `json:"file_content_type,omitempty"`
	FileSize         int64  `json:"file_size,omitempty"`
	FileSHA256       string `json:"file_sha256,omitempty"`
	HasFileAttachment bool   `json:"has_file_attachment,omitempty"`
}

// EnqueueRecordingRequest sends a recording request to SQS
func (s *SQSClient) EnqueueRecordingRequest(req *RecordingRequest) error {
	// Prepare message payload
	messageData, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %v", err)
	}

	messageBody := string(messageData)

	// Check message size (limit is 1024KB = 1MB)
	messageSizeKB := len(messageBody) / 1024
	if messageSizeKB > 512 {
		logging.Logger.Warn(fmt.Sprintf("Large message size: %dKB for URL '%s'. Limit is 1024KB.", messageSizeKB, req.URL))
	}
	if messageSizeKB > 1024 {
		return fmt.Errorf("message too large (%dKB) for queue (1024KB limit)", messageSizeKB)
	}

	// Prepare message attributes for filtering/routing
	messageAttributes := map[string]*sqs.MessageAttributeValue{
		"timestamp": {
			DataType:    aws.String("String"),
			StringValue: aws.String(req.Timestamp),
		},
	}

	if req.TenantID != "" {
		messageAttributes["tenant_id"] = &sqs.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(req.TenantID),
		}
	}

	// URL attribute (truncated to 256 chars for SQS attribute limit)
	urlAttr := req.URL
	if len(urlAttr) > 256 {
		urlAttr = urlAttr[:253] + "..."
	}
	messageAttributes["url"] = &sqs.MessageAttributeValue{
		DataType:    aws.String("String"),
		StringValue: aws.String(urlAttr),
	}

	// Send message with timeout protection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = s.client.SendMessageWithContext(ctx, &sqs.SendMessageInput{
		QueueUrl:          aws.String(s.queueURL),
		MessageBody:       aws.String(messageBody),
		MessageAttributes: messageAttributes,
	})

	if err != nil {
		logging.Logger.Error(fmt.Sprintf("SQS enqueue failed: %v. Queue URL: %s", err, s.queueURL))
		return fmt.Errorf("SQS enqueue failed: %v", err)
	}

	logging.Logger.Info(fmt.Sprintf("Successfully enqueued recording request to SQS for URL '%s'", req.URL))
	return nil
}
