package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var ErrClaimed = errors.New("instance is already claimed")

var ErrInvalidInvite = errors.New("invite is invalid, expired, or already used")

var (
	ErrSelfDelete      = errors.New("the current owner cannot be deleted")
	ErrUnauthenticated = errors.New("session is missing, expired, or revoked")
)

type Config struct {
	DataDir         string
	MaxUploadSize   int64
	Now             func() time.Time
	SkipObjectPrune bool
}

type Store struct {
	db      *sql.DB
	now     func() time.Time
	dataDir string
	maxSize int64
	fileMu  sync.Mutex
}

type Instance struct {
	Claimed  bool
	SiteName string
}

type Owner struct {
	ID             string
	UserHandle     []byte
	CredentialID   []byte
	PersonName     string
	PasskeyName    string
	CredentialJSON []byte
	AAGUID         string
	Attachment     string
	Transports     []string
	BackupEligible bool
	BackupState    bool
	UserAgent      string
	SignCount      uint32
	CloneWarning   bool
	CreatedAt      time.Time
	LastUsedAt     *time.Time
}

func Open(ctx context.Context, cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.DataDir) == "" {
		return nil, errors.New("data directory is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxUploadSize == 0 {
		cfg.MaxUploadSize = 100 << 20
	}
	if cfg.MaxUploadSize < 1 {
		return nil, errors.New("maximum upload size must be positive")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	dbURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(cfg.DataDir, "capsule.db"))}).String()
	dsn := dbURL + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	store := &Store{db: db, now: cfg.Now, dataDir: cfg.DataDir, maxSize: cfg.MaxUploadSize}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if !cfg.SkipObjectPrune {
		if err := store.pruneOrphanObjects(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
		if err := store.pruneTempUploads(); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	const migrationTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);`
	migrations := []string{`
CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS owners (
    id TEXT PRIMARY KEY,
    user_handle BLOB NOT NULL UNIQUE,
	credential_id BLOB NOT NULL UNIQUE,
    person_name TEXT NOT NULL,
    passkey_name TEXT NOT NULL,
    credential_json BLOB NOT NULL,
	aaguid TEXT NOT NULL DEFAULT '',
	authenticator_attachment TEXT NOT NULL DEFAULT '',
	transports_json TEXT NOT NULL DEFAULT '[]',
	backup_eligible INTEGER NOT NULL DEFAULT 0,
	backup_state INTEGER NOT NULL DEFAULT 0,
	user_agent TEXT NOT NULL DEFAULT '',
	sign_count INTEGER NOT NULL DEFAULT 0,
	clone_warning INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    last_used_at TEXT
);
CREATE TABLE IF NOT EXISTS files (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL COLLATE NOCASE UNIQUE,
    content_type TEXT NOT NULL,
    size INTEGER NOT NULL,
    sha256 TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS invites (
    token_hash BLOB PRIMARY KEY,
    created_by TEXT NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    consumed_at TEXT
);
CREATE TABLE IF NOT EXISTS sessions (
    token_hash BLOB PRIMARY KEY,
    owner_id TEXT NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
    csrf_token TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);`}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sqlite migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, migrationTable); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, len(migrations))
	}
	for version < len(migrations) {
		if _, err := tx.ExecContext(ctx, migrations[version]); err != nil {
			return fmt.Errorf("apply sqlite migration %d: %w", version+1, err)
		}
		version++
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, version, formatTime(s.now())); err != nil {
			return fmt.Errorf("record sqlite migration %d: %w", version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sqlite migrations: %w", err)
	}
	return nil
}

func (s *Store) Claim(ctx context.Context, siteName string, owner Owner) error {
	siteName = strings.TrimSpace(siteName)
	owner.PersonName = strings.TrimSpace(owner.PersonName)
	owner.PasskeyName = strings.TrimSpace(owner.PasskeyName)
	if siteName == "" || !validOwner(owner) {
		return errors.New("site name and complete owner are required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES ('claimed', '1')`); err != nil {
		if isConstraintError(err) {
			return ErrClaimed
		}
		return fmt.Errorf("reserve claim: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES ('site_name', ?)`, siteName); err != nil {
		return fmt.Errorf("save site name: %w", err)
	}
	createdAt := s.now().UTC()
	if err := insertOwner(ctx, tx, owner, createdAt); err != nil {
		return fmt.Errorf("save first owner: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit claim: %w", err)
	}
	return nil
}

func (s *Store) Instance(ctx context.Context) (Instance, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings WHERE key IN ('claimed', 'site_name')`)
	if err != nil {
		return Instance{}, fmt.Errorf("read instance: %w", err)
	}
	defer rows.Close()

	var instance Instance
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return Instance{}, fmt.Errorf("scan instance: %w", err)
		}
		switch key {
		case "claimed":
			instance.Claimed = value == "1"
		case "site_name":
			instance.SiteName = value
		}
	}
	return instance, rows.Err()
}

func (s *Store) Owners(ctx context.Context) ([]Owner, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, user_handle, credential_id, person_name, passkey_name, credential_json,
       aaguid, authenticator_attachment, transports_json, backup_eligible, backup_state,
       user_agent, sign_count, clone_warning, created_at, last_used_at
FROM owners ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list owners: %w", err)
	}
	defer rows.Close()

	var owners []Owner
	for rows.Next() {
		owner, err := scanOwner(rows)
		if err != nil {
			return nil, fmt.Errorf("scan owner: %w", err)
		}
		owners = append(owners, owner)
	}
	return owners, rows.Err()
}

type Invite struct {
	Token     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

func (s *Store) CreateInvite(ctx context.Context, ownerID string) (Invite, error) {
	token, err := randomID(32)
	if err != nil {
		return Invite{}, err
	}
	createdAt := s.now().UTC()
	invite := Invite{Token: token, CreatedAt: createdAt, ExpiresAt: createdAt.Add(6 * time.Hour)}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO invites(token_hash, created_by, created_at, expires_at)
VALUES (?, ?, ?, ?)`, hashToken(token), ownerID, formatTime(invite.CreatedAt), formatTime(invite.ExpiresAt)); err != nil {
		return Invite{}, fmt.Errorf("create invite: %w", err)
	}
	return invite, nil
}

func (s *Store) AcceptInvite(ctx context.Context, token string, owner Owner) error {
	owner.PersonName = strings.TrimSpace(owner.PersonName)
	owner.PasskeyName = strings.TrimSpace(owner.PasskeyName)
	if !validOwner(owner) {
		return errors.New("complete owner is required")
	}

	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin invite acceptance: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var expiresAtValue string
	err = tx.QueryRowContext(ctx, `
UPDATE invites SET consumed_at = ?
WHERE token_hash = ? AND consumed_at IS NULL
RETURNING expires_at`, formatTime(now), hashToken(token)).Scan(&expiresAtValue)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidInvite
	}
	if err != nil {
		return fmt.Errorf("consume invite: %w", err)
	}
	expiresAt, err := parseTime(expiresAtValue)
	if err != nil {
		return fmt.Errorf("parse invite expiry: %w", err)
	}
	if !now.Before(expiresAt) {
		return ErrInvalidInvite
	}
	if err := insertOwner(ctx, tx, owner, now); err != nil {
		if isConstraintError(err) {
			return ErrInvalidInvite
		}
		return fmt.Errorf("save invited owner: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit invite acceptance: %w", err)
	}
	return nil
}

type sqlExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertOwner(ctx context.Context, exec sqlExecer, owner Owner, createdAt time.Time) error {
	transports, err := json.Marshal(owner.Transports)
	if err != nil {
		return fmt.Errorf("encode transports: %w", err)
	}
	_, err = exec.ExecContext(ctx, `
INSERT INTO owners(
    id, user_handle, credential_id, person_name, passkey_name, credential_json,
    aaguid, authenticator_attachment, transports_json, backup_eligible, backup_state,
    user_agent, sign_count, clone_warning, created_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		owner.ID, owner.UserHandle, owner.CredentialID, owner.PersonName, owner.PasskeyName, owner.CredentialJSON,
		owner.AAGUID, owner.Attachment, string(transports), owner.BackupEligible, owner.BackupState,
		owner.UserAgent, owner.SignCount, owner.CloneWarning, formatTime(createdAt))
	return err
}

func validOwner(owner Owner) bool {
	return owner.ID != "" && len(owner.UserHandle) != 0 && len(owner.CredentialID) != 0 &&
		owner.PersonName != "" && owner.PasskeyName != "" && len(owner.CredentialJSON) != 0
}

func hashToken(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

func (s *Store) OwnerByUserHandle(ctx context.Context, userHandle []byte) (Owner, error) {
	return s.ownerBy(ctx, `user_handle = ?`, userHandle)
}

func (s *Store) OwnerByID(ctx context.Context, id string) (Owner, error) {
	return s.ownerBy(ctx, `id = ?`, id)
}

func (s *Store) ownerBy(ctx context.Context, predicate string, value any) (Owner, error) {
	query := `
SELECT id, user_handle, credential_id, person_name, passkey_name, credential_json,
       aaguid, authenticator_attachment, transports_json, backup_eligible, backup_state,
       user_agent, sign_count, clone_warning, created_at, last_used_at
FROM owners WHERE ` + predicate
	owner, err := scanOwner(s.db.QueryRowContext(ctx, query, value))
	if errors.Is(err, sql.ErrNoRows) {
		return Owner{}, ErrNotFound
	}
	if err != nil {
		return Owner{}, fmt.Errorf("find owner: %w", err)
	}
	return owner, nil
}

func scanOwner(row rowScanner) (Owner, error) {
	var owner Owner
	var transportsJSON string
	var createdAt string
	var lastUsedAt sql.NullString
	if err := row.Scan(
		&owner.ID, &owner.UserHandle, &owner.CredentialID, &owner.PersonName, &owner.PasskeyName, &owner.CredentialJSON,
		&owner.AAGUID, &owner.Attachment, &transportsJSON, &owner.BackupEligible, &owner.BackupState,
		&owner.UserAgent, &owner.SignCount, &owner.CloneWarning, &createdAt, &lastUsedAt,
	); err != nil {
		return Owner{}, err
	}
	if err := json.Unmarshal([]byte(transportsJSON), &owner.Transports); err != nil {
		return Owner{}, fmt.Errorf("decode transports: %w", err)
	}
	var err error
	owner.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return Owner{}, err
	}
	if lastUsedAt.Valid {
		parsed, parseErr := parseTime(lastUsedAt.String)
		if parseErr != nil {
			return Owner{}, parseErr
		}
		owner.LastUsedAt = &parsed
	}
	return owner, nil
}

func isConstraintError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "constraint") || strings.Contains(message, "unique")
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}
