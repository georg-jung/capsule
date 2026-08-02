package store

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("name already exists")
	ErrFileTooLarge = errors.New("file is too large")
)

type File struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	ContentType string    `json:"contentType"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func (s *Store) PutFile(ctx context.Context, name string, source io.Reader) (File, bool, error) {
	if err := validateFilename(name); err != nil {
		return File{}, false, err
	}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()

	tempDir := filepath.Join(s.dataDir, "tmp")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return File{}, false, fmt.Errorf("create upload directory: %w", err)
	}
	temp, err := os.CreateTemp(tempDir, "upload-*")
	if err != nil {
		return File{}, false, fmt.Errorf("create temporary upload: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()

	limited := io.LimitReader(source, s.maxSize+1)
	buffered := bufio.NewReader(limited)
	peek, _ := buffered.Peek(512)
	contentType := contentTypeFor(name, peek)
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(temp, hash), buffered)
	if copyErr != nil {
		_ = temp.Close()
		return File{}, false, fmt.Errorf("write upload: %w", copyErr)
	}
	if size > s.maxSize {
		_ = temp.Close()
		return File{}, false, ErrFileTooLarge
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return File{}, false, fmt.Errorf("sync upload: %w", err)
	}
	if err := temp.Close(); err != nil {
		return File{}, false, fmt.Errorf("close upload: %w", err)
	}

	digest := hex.EncodeToString(hash.Sum(nil))
	objectPath, err := s.objectPath(digest)
	if err != nil {
		return File{}, false, err
	}
	if err := os.MkdirAll(filepath.Dir(objectPath), 0o700); err != nil {
		return File{}, false, fmt.Errorf("create object directory: %w", err)
	}
	if _, err := os.Stat(objectPath); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(tempName, objectPath); err != nil {
			return File{}, false, fmt.Errorf("commit file object: %w", err)
		}
	} else if err != nil {
		return File{}, false, fmt.Errorf("inspect file object: %w", err)
	}

	var previousDigest sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT sha256 FROM files WHERE name = ?`, name).Scan(&previousDigest); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return File{}, false, fmt.Errorf("inspect existing file: %w", err)
	}

	id, err := randomID(18)
	if err != nil {
		return File{}, false, err
	}
	now := s.now().UTC()
	row := s.db.QueryRowContext(ctx, `
INSERT INTO files(id, name, content_type, size, sha256, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
    name = excluded.name,
    content_type = excluded.content_type,
    size = excluded.size,
    sha256 = excluded.sha256,
    updated_at = excluded.updated_at
RETURNING id, name, content_type, size, sha256, created_at, updated_at`,
		id, name, contentType, size, digest, formatTime(now), formatTime(now))
	file, err := scanFile(row)
	if err != nil {
		return File{}, false, fmt.Errorf("save file: %w", err)
	}
	if previousDigest.Valid && previousDigest.String != digest {
		_ = s.removeObjectIfUnreferenced(ctx, previousDigest.String)
	}
	return file, file.ID != id, nil
}

func (s *Store) Files(ctx context.Context) ([]File, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, content_type, size, sha256, created_at, updated_at
FROM files ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()

	files := make([]File, 0)
	for rows.Next() {
		file, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan file: %w", err)
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

func (s *Store) OpenFile(ctx context.Context, id string) (*os.File, File, error) {
	file, err := scanFile(s.db.QueryRowContext(ctx, `
SELECT id, name, content_type, size, sha256, created_at, updated_at FROM files WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, File{}, ErrNotFound
	}
	if err != nil {
		return nil, File{}, fmt.Errorf("find file: %w", err)
	}
	objectPath, err := s.objectPath(file.SHA256)
	if err != nil {
		return nil, File{}, fmt.Errorf("invalid file object: %w", err)
	}
	opened, err := os.Open(objectPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, File{}, ErrNotFound
	}
	if err != nil {
		return nil, File{}, fmt.Errorf("open file object: %w", err)
	}
	return opened, file, nil
}

func (s *Store) RenameFile(ctx context.Context, id, name string) (File, error) {
	if err := validateFilename(name); err != nil {
		return File{}, err
	}
	opened, _, err := s.OpenFile(ctx, id)
	if err != nil {
		return File{}, err
	}
	sample, readErr := io.ReadAll(io.LimitReader(opened, 512))
	closeErr := opened.Close()
	if readErr != nil {
		return File{}, fmt.Errorf("inspect renamed file: %w", readErr)
	}
	if closeErr != nil {
		return File{}, fmt.Errorf("close renamed file: %w", closeErr)
	}
	contentType := contentTypeFor(name, sample)
	now := s.now().UTC()
	file, err := scanFile(s.db.QueryRowContext(ctx, `
UPDATE files SET name = ?, content_type = ?, updated_at = ? WHERE id = ?
RETURNING id, name, content_type, size, sha256, created_at, updated_at`, name, contentType, formatTime(now), id))
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrNotFound
	}
	if isConstraintError(err) {
		return File{}, ErrConflict
	}
	if err != nil {
		return File{}, fmt.Errorf("rename file: %w", err)
	}
	return file, nil
}

func (s *Store) DeleteFile(ctx context.Context, id string) error {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()

	var digest string
	err := s.db.QueryRowContext(ctx, `DELETE FROM files WHERE id = ? RETURNING sha256`, id).Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("delete file: %w", err)
	}
	_ = s.removeObjectIfUnreferenced(ctx, digest)
	return nil
}

func (s *Store) removeObjectIfUnreferenced(ctx context.Context, digest string) error {
	var references int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE sha256 = ?`, digest).Scan(&references); err != nil {
		return fmt.Errorf("count file object references: %w", err)
	}
	if references != 0 {
		return nil
	}
	objectPath, err := s.objectPath(digest)
	if err != nil {
		return err
	}
	if err := os.Remove(objectPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove unreferenced file object: %w", err)
	}
	return nil
}

func (s *Store) pruneOrphanObjects(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT sha256 FROM files`)
	if err != nil {
		return fmt.Errorf("list referenced file objects: %w", err)
	}
	referenced := make(map[string]struct{})
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan referenced file object: %w", err)
		}
		referenced[digest] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close referenced file objects: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list referenced file objects: %w", err)
	}

	objectsDir := filepath.Join(s.dataDir, "objects")
	if err := os.MkdirAll(objectsDir, 0o700); err != nil {
		return fmt.Errorf("create object directory: %w", err)
	}
	return filepath.WalkDir(objectsDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		digest := entry.Name()
		expectedPath, pathErr := s.objectPath(digest)
		if pathErr != nil || filepath.Clean(expectedPath) != filepath.Clean(path) {
			return nil
		}
		if _, found := referenced[digest]; found {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove orphaned file object: %w", err)
		}
		return nil
	})
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanFile(row rowScanner) (File, error) {
	var file File
	var createdAt, updatedAt string
	if err := row.Scan(&file.ID, &file.Name, &file.ContentType, &file.Size, &file.SHA256, &createdAt, &updatedAt); err != nil {
		return File{}, err
	}
	var err error
	file.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return File{}, err
	}
	file.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return File{}, err
	}
	return file, nil
}

func validateFilename(name string) error {
	if name == "" || strings.TrimSpace(name) != name || len(name) > 200 || !utf8.ValidString(name) {
		return errors.New("filename must be valid, non-empty UTF-8 of at most 200 bytes")
	}
	if name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return errors.New("filename must not contain path separators")
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return errors.New("filename must not contain control characters")
		}
	}
	return nil
}

func contentTypeFor(name string, sample []byte) string {
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if contentType == "" {
		contentType = http.DetectContentType(sample)
	}
	if strings.HasPrefix(contentType, "text/") && !strings.Contains(strings.ToLower(contentType), "charset=") {
		contentType += "; charset=utf-8"
	}
	return contentType
}

func randomID(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate random identifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (s *Store) objectPath(digest string) (string, error) {
	if len(digest) != sha256.Size*2 {
		return "", errors.New("invalid SHA-256 digest length")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", errors.New("invalid SHA-256 digest encoding")
	}
	return filepath.Join(s.dataDir, "objects", digest[:2], digest), nil
}
