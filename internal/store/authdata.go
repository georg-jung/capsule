package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (s *Store) ValidateInvite(ctx context.Context, token string) (Invite, error) {
	var createdAt, expiresAt string
	var consumedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT created_at, expires_at, consumed_at FROM invites WHERE token_hash = ?`, hashToken(token)).Scan(&createdAt, &expiresAt, &consumedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Invite{}, ErrInvalidInvite
	}
	if err != nil {
		return Invite{}, fmt.Errorf("validate invite: %w", err)
	}
	created, err := parseTime(createdAt)
	if err != nil {
		return Invite{}, fmt.Errorf("parse invite creation time: %w", err)
	}
	expires, err := parseTime(expiresAt)
	if err != nil {
		return Invite{}, fmt.Errorf("parse invite expiry: %w", err)
	}
	if consumedAt.Valid || !s.now().UTC().Before(expires) {
		return Invite{}, ErrInvalidInvite
	}
	return Invite{Token: token, CreatedAt: created, ExpiresAt: expires}, nil
}

// sessionTTL is how long a session stays valid after it was last extended.
// sessionRenewAfter is how much of that lifetime must elapse since the last
// extension before Authenticate slides the expiry forward again.
const (
	sessionTTL        = 90 * 24 * time.Hour
	sessionRenewAfter = 24 * time.Hour
)

type SessionCredentials struct {
	Token     string
	CSRFToken string
	ExpiresAt time.Time
}

type Authenticated struct {
	Owner     Owner
	CSRFToken string
	ExpiresAt time.Time
	// Extended reports whether Authenticate slid the session's expiry
	// forward. Callers should re-issue the session cookie when true.
	Extended bool
}

func (s *Store) CreateSession(ctx context.Context, ownerID string) (SessionCredentials, error) {
	token, err := randomID(32)
	if err != nil {
		return SessionCredentials{}, err
	}
	csrf, err := randomID(32)
	if err != nil {
		return SessionCredentials{}, err
	}
	createdAt := s.now().UTC()
	session := SessionCredentials{Token: token, CSRFToken: csrf, ExpiresAt: createdAt.Add(sessionTTL)}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO sessions(token_hash, owner_id, csrf_token, created_at, expires_at)
VALUES (?, ?, ?, ?, ?)`, hashToken(token), ownerID, csrf, formatTime(createdAt), formatTime(session.ExpiresAt)); err != nil {
		return SessionCredentials{}, fmt.Errorf("create session: %w", err)
	}
	return session, nil
}

// Authenticate resolves a session token. When extend is false the session is
// validated but its expiry is left alone, so a request the caller does not
// consider a genuine sign of life cannot postpone inactivity expiry.
func (s *Store) Authenticate(ctx context.Context, token string, extend bool) (Authenticated, error) {
	var authenticated Authenticated
	var ownerID string
	var expiresAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT owner_id, csrf_token, expires_at FROM sessions WHERE token_hash = ?`, hashToken(token)).Scan(
		&ownerID, &authenticated.CSRFToken, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Authenticated{}, ErrUnauthenticated
	}
	if err != nil {
		return Authenticated{}, fmt.Errorf("authenticate session: %w", err)
	}
	if !expiresAt.Valid {
		return Authenticated{}, ErrUnauthenticated
	}
	authenticated.ExpiresAt, err = parseTime(expiresAt.String)
	if err != nil {
		return Authenticated{}, fmt.Errorf("parse session expiry: %w", err)
	}
	if !s.now().UTC().Before(authenticated.ExpiresAt) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
		return Authenticated{}, ErrUnauthenticated
	}
	authenticated.Owner, err = s.OwnerByID(ctx, ownerID)
	if errors.Is(err, ErrNotFound) {
		return Authenticated{}, ErrUnauthenticated
	}
	if err != nil {
		return Authenticated{}, err
	}

	now := s.now().UTC()
	renewalDue := authenticated.ExpiresAt.Before(now.Add(sessionTTL - sessionRenewAfter))
	if extend && renewalDue {
		newExpiresAt := now.Add(sessionTTL)
		if _, execErr := s.db.ExecContext(ctx, `
UPDATE sessions SET expires_at = ? WHERE token_hash = ?`, formatTime(newExpiresAt), hashToken(token)); execErr == nil {
			authenticated.ExpiresAt = newExpiresAt
			authenticated.Extended = true
		}
	}

	return authenticated, nil
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (s *Store) UpdateOwnerCredential(ctx context.Context, owner Owner) error {
	transports, err := json.Marshal(owner.Transports)
	if err != nil {
		return fmt.Errorf("encode transports: %w", err)
	}
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE owners SET
    credential_json = ?, aaguid = ?, authenticator_attachment = ?, transports_json = ?,
    backup_eligible = ?, backup_state = ?, sign_count = ?, clone_warning = ?, last_used_at = ?
WHERE id = ? AND credential_id = ?`,
		owner.CredentialJSON, owner.AAGUID, owner.Attachment, string(transports), owner.BackupEligible,
		owner.BackupState, owner.SignCount, owner.CloneWarning, formatTime(now), owner.ID, owner.CredentialID)
	if err != nil {
		return fmt.Errorf("update owner credential: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count updated owner credentials: %w", err)
	}
	if updated == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RenamePasskey(ctx context.Context, currentOwnerID, targetOwnerID, name string) error {
	if name == "" {
		return errors.New("passkey name is required")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE owners SET passkey_name = ?
WHERE id = ? AND EXISTS (SELECT 1 FROM owners WHERE id = ?)`, name, targetOwnerID, currentOwnerID)
	if err != nil {
		return fmt.Errorf("rename passkey: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count renamed passkeys: %w", err)
	}
	if updated == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UpdateSiteName(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("site name is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE settings SET value = ? WHERE key = 'site_name'`, name)
	if err != nil {
		return fmt.Errorf("update site name: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count updated site names: %w", err)
	}
	if updated == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteOtherOwner(ctx context.Context, currentOwnerID, targetOwnerID string) error {
	if currentOwnerID == targetOwnerID {
		return ErrSelfDelete
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM owners WHERE id = ?`, targetOwnerID)
	if err != nil {
		return fmt.Errorf("delete owner: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count deleted owners: %w", err)
	}
	if deleted == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ResetAuth(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin auth reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	statements := []string{
		`DELETE FROM sessions`,
		`DELETE FROM invites`,
		`DELETE FROM owners`,
		`DELETE FROM settings WHERE key IN ('claimed', 'site_name')`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("reset authentication: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit auth reset: %w", err)
	}
	return nil
}
