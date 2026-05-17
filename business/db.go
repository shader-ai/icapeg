package business

import (
	"context"
	"database/sql"
	"fmt"
	"icapeg/logging"
	"time"

	_ "github.com/lib/pq" // PostgreSQL driver
)

// InitDatabase initializes database connection
func InitDatabase(databaseURL string) (*sql.DB, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("database URL not configured")
	}

	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open database connection: %v", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %v", err)
	}

	logging.Logger.Info("Database connection established successfully")
	return db, nil
}
