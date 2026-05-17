package server

import (
	"fmt"
	"icapeg/business"
	"icapeg/logging"
	http_server "icapeg/server/http-server"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"icapeg/api"
	"icapeg/config"
	"icapeg/icap"

	"github.com/joho/godotenv"
)

// https://github.com/k8-proxy/k8-rebuild-rest-api
// StartServer starts the icap server

func StartServer() error {
	// Load .env before config — later merging overrides duplicate keys (last file wins).
	var loadedEnvFiles []string
	for _, p := range []string{".env", filepath.Join("..", ".env"), filepath.Join("icapeg", ".env")} {
		if err := godotenv.Load(p); err == nil {
			loadedEnvFiles = append(loadedEnvFiles, p)
		}
	}

	// any request even the service doesn't exist in toml file, it will go to api.ToICAPEGServe
	// and there, the request will be filtered to check if the service exists or not

	config.Init()
	if len(loadedEnvFiles) > 0 {
		logging.Logger.Info(fmt.Sprintf("Loaded environment file(s): %s", strings.Join(loadedEnvFiles, ", ")))
	}

	// Initialize business logic handler (tenant validation, URL matching, SQS)
	// Read configuration from environment variables or config file
	databaseURL := os.Getenv("DATABASE_URL")
	sqsQueueURL := os.Getenv("RECORDING_QUEUE_URL")
	sqsRegion := os.Getenv("AWS_REGION")
	if sqsRegion == "" {
		sqsRegion = "us-east-1" // default region
	}
	sqsAccessKeyID := os.Getenv("AWS_ACCESS_KEY_ID")
	sqsSecretAccessKey := os.Getenv("AWS_SECRET_ACCESS_KEY")

	if err := business.Init(databaseURL, sqsQueueURL, sqsRegion, sqsAccessKeyID, sqsSecretAccessKey); err != nil {
		logging.Logger.Warn(fmt.Sprintf("Failed to initialize business logic: %v. Continuing without business logic features.", err))
	}
	logRecordingPipelineEnv(databaseURL, sqsQueueURL)

	//HTTP server
	htmlWebServer := http.NewServeMux()
	htmlWebServer.HandleFunc("/service/message", http_server.HtmlMessage)
	go func() {
		http.ListenAndServe(":8081", htmlWebServer)
	}()

	icap.HandleFunc("/", api.ToICAPEGServe)

	logging.Logger.Info("starting the ICAP server")

	stop := make(chan os.Signal, 1)

	signal.Notify(stop, syscall.SIGKILL, syscall.SIGINT, syscall.SIGQUIT)

	go func() {
		if err := icap.ListenAndServe(fmt.Sprintf(":%d", config.App().Port), nil); err != nil {
			logging.Logger.Fatal(err.Error())
		}
	}()

	ticker := time.NewTicker(10 * time.Second)
	go func() {
		for {
			select {
			case _ = <-ticker.C:
			}
		}
	}()

	time.Sleep(5 * time.Millisecond)
	port := strconv.Itoa(config.App().Port)
	logging.Logger.Info("ICAP server is running on localhost: " + port)

	<-stop
	ticker.Stop()

	logging.Logger.Info("ICAP server gracefully shut down")

	return nil
}

// logRecordingPipelineEnv prints one startup summary so operators can see why recording might be off.
func logRecordingPipelineEnv(databaseURL, sqsQueueURL string) {
	h := business.GetHandler()
	if h == nil {
		logging.Logger.Warn("ICAP recording: DISABLED — business logic handler not initialized. " +
			"Set DATABASE_URL (same Postgres as backend), ensure the host is reachable from this process " +
			"(e.g. postgresql://...@host.docker.internal:5432/... when Postgres runs on the host). See icapeg/.env.example.")
		return
	}
	logging.Logger.Info("ICAP recording: database + endpoint cache OK (URL matching active when TENANT_ID is set)")
	if strings.TrimSpace(os.Getenv("TENANT_ID")) == "" {
		logging.Logger.Warn("ICAP recording: TENANT_ID is unset — set it to your tenants.id UUID (same tenant as gateway)")
	}
	if strings.TrimSpace(databaseURL) == "" {
		logging.Logger.Warn("ICAP recording: DATABASE_URL was empty at startup (unexpected if handler is non-nil)")
	}
	if strings.TrimSpace(sqsQueueURL) == "" {
		logging.Logger.Warn("ICAP recording: RECORDING_QUEUE_URL is unset — matched requests will not enqueue to SQS (set same queue URL as backend worker)")
	} else {
		logging.Logger.Info("ICAP recording: RECORDING_QUEUE_URL is set — SQS enqueue enabled when a catalog rule matches")
	}
}
