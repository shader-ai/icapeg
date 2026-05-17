package business

import (
	"database/sql"
	"fmt"
	"icapeg/logging"
	"sync"
	"time"
)

var (
	businessHandler *BusinessLogicHandler
	initOnce        sync.Once
	db              *sql.DB
)

const endpointCacheRefreshInterval = 5 * time.Minute

// Init initializes the business logic handler with database and SQS configuration.
// Endpoint cache (ai_tool_endpoints) is loaded at startup and refreshed periodically.
func Init(databaseURL, sqsQueueURL, sqsRegion, sqsAccessKeyID, sqsSecretAccessKey string) error {
	var initErr error
	initOnce.Do(func() {
		// Initialize database connection
		if databaseURL != "" {
			var err error
			db, err = InitDatabase(databaseURL)
			if err != nil {
				logging.Logger.Error(fmt.Sprintf("Failed to initialize database: %v. Business logic features will be disabled.", err))
				initErr = err
				return
			}
		} else {
			logging.Logger.Warn("Database URL not configured. Business logic features will be disabled.")
		}

		// Initialize business logic handler (this loads the endpoint cache via NewURLMatcher)
		if db != nil {
			handler, err := NewBusinessLogicHandler(db, sqsQueueURL, sqsRegion, sqsAccessKeyID, sqsSecretAccessKey)
			if err != nil {
				logging.Logger.Warn(fmt.Sprintf("Failed to initialize business logic handler: %v", err))
			}
			businessHandler = handler
			// Periodic refresh of ai_tool_endpoints cache
			go func() {
				ticker := time.NewTicker(endpointCacheRefreshInterval)
				defer ticker.Stop()
				for range ticker.C {
					if h := GetHandler(); h != nil {
						h.RefreshEndpointCache()
					}
				}
			}()
		}
	})
	return initErr
}

// GetHandler returns the initialized business logic handler
func GetHandler() *BusinessLogicHandler {
	return businessHandler
}

// GetDB returns the database connection
func GetDB() *sql.DB {
	return db
}
