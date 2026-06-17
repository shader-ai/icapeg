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

// Init initializes the business logic handler with database, SQS, S3, and LDAP configuration.
// Endpoint cache (ai_tool_endpoints) is loaded at startup and refreshed periodically.
func Init(databaseURL, sqsQueueURL, sqsRegion, sqsAccessKeyID, sqsSecretAccessKey, s3Bucket string) error {
	return InitWithLDAP(databaseURL, sqsQueueURL, sqsRegion, sqsAccessKeyID, sqsSecretAccessKey, s3Bucket, "", "", "", "", "")
}

// InitWithLDAP initializes the business logic handler including an optional LDAP enricher.
// Pass empty strings for LDAP params to disable enrichment.
func InitWithLDAP(databaseURL, sqsQueueURL, sqsRegion, sqsAccessKeyID, sqsSecretAccessKey, s3Bucket,
	ldapHost, ldapPort, ldapBaseDN, ldapBindDN, ldapBindPassword string) error {
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

		// Build optional LDAP enricher
		var enricher *LDAPEnricher
		if ldapHost != "" && ldapBindDN != "" && ldapBindPassword != "" {
			if ldapPort == "" {
				ldapPort = "389"
			}
			enricher = NewLDAPEnricher(ldapHost, ldapPort, ldapBaseDN, ldapBindDN, ldapBindPassword)
			logging.Logger.Info(fmt.Sprintf("LDAP enricher initialized: %s:%s base=%q", ldapHost, ldapPort, ldapBaseDN))
		} else {
			logging.Logger.Warn("LDAP_HOST / LDAP_BIND_DN / LDAP_BIND_PASSWORD not set — AD enrichment disabled")
		}

		// Initialize business logic handler (this loads the endpoint cache via NewURLMatcher)
		if db != nil {
			handler, err := NewBusinessLogicHandler(db, sqsQueueURL, sqsRegion, sqsAccessKeyID, sqsSecretAccessKey, s3Bucket, enricher)
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
