package business

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"icapeg/logging"
	"time"
)

// TenantValidator handles tenant ID validation
type TenantValidator struct {
	db *sql.DB
}

// NewTenantValidator creates a new tenant validator
func NewTenantValidator(db *sql.DB) *TenantValidator {
	return &TenantValidator{db: db}
}

// ValidateTenant validates that a tenant exists in the database
func (tv *TenantValidator) ValidateTenant(tenantID string) (bool, error) {
	if tenantID == "" {
		return false, errors.New("missing or invalid tenant_id")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var id string
	query := "SELECT id FROM tenants WHERE id = $1"
	err := tv.db.QueryRowContext(ctx, query, tenantID).Scan(&id)
	
	if err == sql.ErrNoRows {
		logging.Logger.Debug(fmt.Sprintf("Tenant '%s' not found", tenantID))
		return false, fmt.Errorf("tenant '%s' not found", tenantID)
	}
	if err != nil {
		logging.Logger.Error(fmt.Sprintf("Error checking tenant existence: %v", err))
		return false, fmt.Errorf("database error: %v", err)
	}

	return true, nil
}

// ValidateTenantAndUser validates tenant and user existence
func (tv *TenantValidator) ValidateTenantAndUser(tenantID, userID string) (bool, error) {
	// First validate tenant
	isValid, err := tv.ValidateTenant(tenantID)
	if !isValid {
		return false, err
	}

	if userID == "" {
		// Align with IdentityExtractor: user is optional when proxy headers are missing.
		return true, nil
	}

	// Check if user exists (non-blocking - continue even if fails)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var id string
	query := "SELECT id FROM tenant_users WHERE tenant_id = $1 AND identity_id = $2"
	err = tv.db.QueryRowContext(ctx, query, tenantID, userID).Scan(&id)
	
	if err == sql.ErrNoRows {
		logging.Logger.Debug(fmt.Sprintf("Tenant user not found: tenant_id=%s, identity_id=%s", tenantID, userID))
		// User not found is not a blocking error - continue
		return true, nil
	}
	if err != nil {
		logging.Logger.Warn(fmt.Sprintf("Error looking up user: %v - continuing", err))
		// Continue even on error
		return true, nil
	}

	// Update last activity if user exists
	updateQuery := `UPDATE tenant_users 
		SET last_activity_at = $1, updated_at = $1 
		WHERE tenant_id = $2 AND identity_id = $3`
	_, err = tv.db.ExecContext(ctx, updateQuery, time.Now().UTC(), tenantID, userID)
	if err != nil {
		logging.Logger.Warn(fmt.Sprintf("Error updating user activity: %v", err))
	}

	return true, nil
}
