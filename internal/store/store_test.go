package store_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/georg-jung/capsule/internal/store"
)

func TestExistingVersionZeroDatabaseMigratesInPlace(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	databaseURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(dataDir, "capsule.db"))}).String()
	database, err := sql.Open("sqlite", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO settings(key, value) VALUES ('legacy_marker', 'preserved');`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err := store.Open(context.Background(), store.Config{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = sql.Open("sqlite", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var version int
	if err := database.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	var marker string
	if err := database.QueryRow(`SELECT value FROM settings WHERE key = 'legacy_marker'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if version != 1 || marker != "preserved" {
		t.Fatalf("version = %d, marker = %q", version, marker)
	}
}

func TestFirstOwnerClaimsInstanceExactlyOnce(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	repo, err := store.Open(context.Background(), store.Config{
		DataDir: t.TempDir(),
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	first := store.Owner{
		ID:             "owner-one",
		UserHandle:     []byte("owner-one-handle"),
		CredentialID:   []byte("credential-one"),
		PersonName:     "Georg",
		PasskeyName:    "Windows Hello",
		CredentialJSON: []byte(`{"id":"credential-one"}`),
	}
	if err := repo.Claim(context.Background(), "My tools", first); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	second := first
	second.ID = "owner-two"
	second.UserHandle = []byte("owner-two-handle")
	second.CredentialID = []byte("credential-two")
	second.CredentialJSON = []byte(`{"id":"credential-two"}`)
	if err := repo.Claim(context.Background(), "Attacker's site", second); !errors.Is(err, store.ErrClaimed) {
		t.Fatalf("second claim error = %v, want ErrClaimed", err)
	}

	instance, err := repo.Instance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !instance.Claimed || instance.SiteName != "My tools" {
		t.Fatalf("instance = %#v", instance)
	}

	owners, err := repo.Owners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 1 || owners[0].ID != first.ID || !owners[0].CreatedAt.Equal(now) {
		t.Fatalf("owners = %#v", owners)
	}
}

func TestConcurrentClaimsHaveExactlyOneWinner(t *testing.T) {
	t.Parallel()

	repo, err := store.Open(context.Background(), store.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	owners := []store.Owner{
		{
			ID: "owner-one", UserHandle: []byte("handle-one"), CredentialID: []byte("credential-one"),
			PersonName: "Georg", PasskeyName: "Windows Hello", CredentialJSON: []byte(`{"id":"credential-one"}`),
		},
		{
			ID: "owner-two", UserHandle: []byte("handle-two"), CredentialID: []byte("credential-two"),
			PersonName: "Peter", PasskeyName: "Google Password Manager", CredentialJSON: []byte(`{"id":"credential-two"}`),
		},
	}

	start := make(chan struct{})
	results := make(chan error, len(owners))
	var group sync.WaitGroup
	for index, owner := range owners {
		group.Add(1)
		go func(siteName string, candidate store.Owner) {
			defer group.Done()
			<-start
			results <- repo.Claim(context.Background(), siteName, candidate)
		}("Site "+string(rune('A'+index)), owner)
	}
	close(start)
	group.Wait()
	close(results)

	successes, alreadyClaimed := 0, 0
	for result := range results {
		switch {
		case result == nil:
			successes++
		case errors.Is(result, store.ErrClaimed):
			alreadyClaimed++
		default:
			t.Fatalf("unexpected claim result: %v", result)
		}
	}
	if successes != 1 || alreadyClaimed != 1 {
		t.Fatalf("successes = %d, already claimed = %d", successes, alreadyClaimed)
	}
}

func TestInviteCanRegisterOneOwnerWithinSixHours(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	repo, err := store.Open(context.Background(), store.Config{
		DataDir: t.TempDir(),
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	first := store.Owner{
		ID:             "owner-one",
		UserHandle:     []byte("owner-one-handle"),
		CredentialID:   []byte("credential-one"),
		PersonName:     "Georg",
		PasskeyName:    "Windows Hello",
		CredentialJSON: []byte(`{"id":"credential-one"}`),
	}
	if err := repo.Claim(context.Background(), "My tools", first); err != nil {
		t.Fatal(err)
	}

	invite, err := repo.CreateInvite(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if invite.Token == "" || !invite.ExpiresAt.Equal(now.Add(6*time.Hour)) {
		t.Fatalf("invite = %#v", invite)
	}

	now = now.Add(6*time.Hour - time.Nanosecond)
	second := store.Owner{
		ID:             "owner-two",
		UserHandle:     []byte("owner-two-handle"),
		CredentialID:   []byte("credential-two"),
		PersonName:     "Peter",
		PasskeyName:    "Google Password Manager",
		CredentialJSON: []byte(`{"id":"credential-two"}`),
	}
	if err := repo.AcceptInvite(context.Background(), invite.Token, second); err != nil {
		t.Fatalf("accept valid invite: %v", err)
	}
	if err := repo.AcceptInvite(context.Background(), invite.Token, second); !errors.Is(err, store.ErrInvalidInvite) {
		t.Fatalf("replay error = %v, want ErrInvalidInvite", err)
	}

	expiring, err := repo.CreateInvite(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	now = expiring.ExpiresAt
	third := second
	third.ID = "owner-three"
	third.UserHandle = []byte("owner-three-handle")
	third.CredentialID = []byte("credential-three")
	if err := repo.AcceptInvite(context.Background(), expiring.Token, third); !errors.Is(err, store.ErrInvalidInvite) {
		t.Fatalf("expiry boundary error = %v, want ErrInvalidInvite", err)
	}

	now = time.Date(2026, time.August, 3, 0, 0, 0, 0, time.UTC)
	justExpired, err := repo.CreateInvite(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	now = justExpired.ExpiresAt.Add(time.Nanosecond)
	fourth := third
	fourth.ID = "owner-four"
	fourth.UserHandle = []byte("owner-four-handle")
	fourth.CredentialID = []byte("credential-four")
	if err := repo.AcceptInvite(context.Background(), justExpired.Token, fourth); !errors.Is(err, store.ErrInvalidInvite) {
		t.Fatalf("just-expired error = %v, want ErrInvalidInvite", err)
	}
}

func TestConcurrentInviteAcceptanceHasExactlyOneWinner(t *testing.T) {
	t.Parallel()

	repo, err := store.Open(context.Background(), store.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	first := store.Owner{
		ID: "owner-one", UserHandle: []byte("handle-one"), CredentialID: []byte("credential-one"),
		PersonName: "Georg", PasskeyName: "Windows Hello", CredentialJSON: []byte(`{"id":"credential-one"}`),
	}
	if err := repo.Claim(context.Background(), "My tools", first); err != nil {
		t.Fatal(err)
	}
	invite, err := repo.CreateInvite(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	candidates := []store.Owner{
		{
			ID: "owner-two", UserHandle: []byte("handle-two"), CredentialID: []byte("credential-two"),
			PersonName: "Peter", PasskeyName: "Google Password Manager", CredentialJSON: []byte(`{"id":"credential-two"}`),
		},
		{
			ID: "owner-three", UserHandle: []byte("handle-three"), CredentialID: []byte("credential-three"),
			PersonName: "Ada", PasskeyName: "iCloud Keychain", CredentialJSON: []byte(`{"id":"credential-three"}`),
		},
	}

	start := make(chan struct{})
	results := make(chan error, len(candidates))
	var group sync.WaitGroup
	for _, candidate := range candidates {
		group.Add(1)
		go func(owner store.Owner) {
			defer group.Done()
			<-start
			results <- repo.AcceptInvite(context.Background(), invite.Token, owner)
		}(candidate)
	}
	close(start)
	group.Wait()
	close(results)

	successes, rejected := 0, 0
	for result := range results {
		switch {
		case result == nil:
			successes++
		case errors.Is(result, store.ErrInvalidInvite):
			rejected++
		default:
			t.Fatalf("unexpected acceptance result: %v", result)
		}
	}
	if successes != 1 || rejected != 1 {
		t.Fatalf("successes = %d, rejected = %d", successes, rejected)
	}
}

func TestFilesReplaceByNameAndKeepStableIdentity(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	dataDir := t.TempDir()
	repo, err := store.Open(context.Background(), store.Config{
		DataDir:       dataDir,
		MaxUploadSize: 1024,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	first, replaced, err := repo.PutFile(context.Background(), "tool.html", strings.NewReader("<h1>one</h1>"))
	if err != nil {
		t.Fatal(err)
	}
	if replaced {
		t.Fatal("first upload reported a replacement")
	}

	now = now.Add(time.Minute)
	second, replaced, err := repo.PutFile(context.Background(), "tool.html", strings.NewReader("<h1>two</h1>"))
	if err != nil {
		t.Fatal(err)
	}
	if !replaced || second.ID != first.ID || !second.CreatedAt.Equal(first.CreatedAt) || !second.UpdatedAt.Equal(now) {
		t.Fatalf("replacement = %#v, replaced = %v; first = %#v", second, replaced, first)
	}

	opened, metadata, err := repo.OpenFile(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	contents, err := io.ReadAll(opened)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "<h1>two</h1>" || metadata.SHA256 == first.SHA256 {
		t.Fatalf("contents = %q, metadata = %#v", contents, metadata)
	}
	if objects := countObjects(t, dataDir); objects != 1 {
		t.Fatalf("object count after replacement = %d, want 1", objects)
	}

	renamed, err := repo.RenameFile(context.Background(), first.ID, "renamed.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(renamed.ContentType, "text/plain") {
		t.Fatalf("renamed content type = %q, want text/plain", renamed.ContentType)
	}
	files, err := repo.Files(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "renamed.txt" {
		t.Fatalf("files = %#v", files)
	}

	if err := repo.DeleteFile(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.OpenFile(context.Background(), first.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("open deleted file error = %v, want ErrNotFound", err)
	}
	if objects := countObjects(t, dataDir); objects != 0 {
		t.Fatalf("object count after deletion = %d, want 0", objects)
	}
}

func TestCompressibleUploadsGetGzipSidecarsThatShareTheObjectLifecycle(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	repo, err := store.Open(context.Background(), store.Config{DataDir: dataDir, MaxUploadSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	body := "<!doctype html><title>capsule</title>" + strings.Repeat("<p>a highly compressible paragraph</p>", 200)
	page, _, err := repo.PutFile(context.Background(), "page.html", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if objects := countObjects(t, dataDir); objects != 2 {
		t.Fatalf("object count after a compressible upload = %d, want 2 (object plus sidecar)", objects)
	}

	compressed, metadata, err := repo.OpenContent(context.Background(), page.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	if compressed.Encoding != "gzip" || compressed.Size >= metadata.Size {
		t.Fatalf("compressed content encoding = %q, size = %d, raw size = %d", compressed.Encoding, compressed.Size, metadata.Size)
	}
	reader, err := gzip.NewReader(compressed)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != body {
		t.Fatal("sidecar does not decode to the uploaded bytes")
	}

	identity, _, err := repo.OpenContent(context.Background(), page.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer identity.Close()
	if identity.Encoding != "" || identity.Size != metadata.Size {
		t.Fatalf("identity content encoding = %q, size = %d, want %d", identity.Encoding, identity.Size, metadata.Size)
	}

	// Random bytes only grow under gzip, so no sidecar is kept for them.
	noise := make([]byte, 64<<10)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	blob, _, err := repo.PutFile(context.Background(), "blob.bin", bytes.NewReader(noise))
	if err != nil {
		t.Fatal(err)
	}
	if objects := countObjects(t, dataDir); objects != 3 {
		t.Fatalf("object count after an incompressible upload = %d, want 3", objects)
	}
	incompressible, _, err := repo.OpenContent(context.Background(), blob.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	defer incompressible.Close()
	if incompressible.Encoding != "" {
		t.Fatalf("incompressible content encoding = %q, want identity", incompressible.Encoding)
	}

	if err := repo.DeleteFile(context.Background(), page.ID); err != nil {
		t.Fatal(err)
	}
	if objects := countObjects(t, dataDir); objects != 1 {
		t.Fatalf("object count after deletion = %d, want 1; the sidecar should go with its object", objects)
	}

	// A sidecar left behind by a crash between the two renames is an orphan
	// like any other object, and startup pruning must reclaim it. Scratch
	// files from an upload that never committed are reclaimed the same way.
	orphan := filepath.Join(dataDir, "objects", page.SHA256[:2], page.SHA256+".gz")
	if err := os.WriteFile(orphan, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	strandedTemp := filepath.Join(dataDir, "tmp", "upload-123456.gz")
	if err := os.WriteFile(strandedTemp, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(context.Background(), store.Config{DataDir: dataDir, MaxUploadSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned sidecar survived startup pruning: %v", err)
	}
	if _, err := os.Stat(strandedTemp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stranded temporary upload survived startup pruning: %v", err)
	}
	if objects := countObjects(t, dataDir); objects != 1 {
		t.Fatalf("object count after pruning = %d, want 1", objects)
	}
}

func TestSessionSlidingRenewal(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	repo, err := store.Open(context.Background(), store.Config{
		DataDir: t.TempDir(),
		Now:     func() time.Time { return clock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	owner := store.Owner{
		ID:             "owner-one",
		UserHandle:     []byte("owner-one-handle"),
		CredentialID:   []byte("credential-one"),
		PersonName:     "Georg",
		PasskeyName:    "Windows Hello",
		CredentialJSON: []byte(`{"id":"credential-one"}`),
	}
	if err := repo.Claim(context.Background(), "My tools", owner); err != nil {
		t.Fatal(err)
	}

	session, err := repo.CreateSession(context.Background(), owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantInitialExpiry := now.Add(90 * 24 * time.Hour)
	if !session.ExpiresAt.Equal(wantInitialExpiry) {
		t.Fatalf("initial expiry = %v, want %v", session.ExpiresAt, wantInitialExpiry)
	}

	// (a) Authenticating within 24h of creation must not slide the expiry.
	now = now.Add(23 * time.Hour)
	authenticated, err := repo.Authenticate(context.Background(), session.Token, true)
	if err != nil {
		t.Fatalf("authenticate within renewal window: %v", err)
	}
	if authenticated.Extended {
		t.Fatal("session should not have been extended within 24h of creation")
	}
	if !authenticated.ExpiresAt.Equal(wantInitialExpiry) {
		t.Fatalf("expiry after early authenticate = %v, want unchanged %v", authenticated.ExpiresAt, wantInitialExpiry)
	}

	// (b) Past the threshold, a request that does not count as a sign of life
	// authenticates but leaves the expiry where it was.
	now = now.Add(2 * time.Hour) // 25h since creation, > sessionRenewAfter (24h)
	authenticated, err = repo.Authenticate(context.Background(), session.Token, false)
	if err != nil {
		t.Fatalf("authenticate without extending: %v", err)
	}
	if authenticated.Extended || !authenticated.ExpiresAt.Equal(wantInitialExpiry) {
		t.Fatalf("non-extending authenticate moved expiry to %v (extended = %v)", authenticated.ExpiresAt, authenticated.Extended)
	}

	// (c) The same request with extend set slides the expiry forward.
	authenticated, err = repo.Authenticate(context.Background(), session.Token, true)
	if err != nil {
		t.Fatalf("authenticate past renewal window: %v", err)
	}
	if !authenticated.Extended {
		t.Fatal("session should have been extended after 24h without contact")
	}
	wantRenewedExpiry := now.Add(90 * 24 * time.Hour)
	if !authenticated.ExpiresAt.Equal(wantRenewedExpiry) {
		t.Fatalf("expiry after renewal = %v, want %v", authenticated.ExpiresAt, wantRenewedExpiry)
	}

	// (d) Without further contact, the session still expires 90 days after the last renewal.
	now = wantRenewedExpiry.Add(time.Nanosecond)
	if _, err := repo.Authenticate(context.Background(), session.Token, true); !errors.Is(err, store.ErrUnauthenticated) {
		t.Fatalf("authenticate after expiry error = %v, want ErrUnauthenticated", err)
	}
}

func countObjects(t *testing.T, dataDir string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(filepath.Join(dataDir, "objects"), func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func TestOwnerDeletionInvalidatesSessionsAndRecoveryPreservesFiles(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	repo, err := store.Open(context.Background(), store.Config{
		DataDir: t.TempDir(),
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	first := store.Owner{
		ID:             "owner-one",
		UserHandle:     []byte("owner-one-handle"),
		CredentialID:   []byte("credential-one"),
		PersonName:     "Georg",
		PasskeyName:    "Windows Hello",
		CredentialJSON: []byte(`{"id":"credential-one"}`),
	}
	if err := repo.Claim(context.Background(), "My tools", first); err != nil {
		t.Fatal(err)
	}
	second := store.Owner{
		ID:             "owner-two",
		UserHandle:     []byte("owner-two-handle"),
		CredentialID:   []byte("credential-two"),
		PersonName:     "Peter",
		PasskeyName:    "Google Password Manager",
		CredentialJSON: []byte(`{"id":"credential-two"}`),
	}
	invite, err := repo.CreateInvite(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.AcceptInvite(context.Background(), invite.Token, second); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.PutFile(context.Background(), "kept.html", strings.NewReader("kept")); err != nil {
		t.Fatal(err)
	}

	firstSession, err := repo.CreateSession(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondSession, err := repo.CreateSession(context.Background(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteOtherOwner(context.Background(), first.ID, first.ID); !errors.Is(err, store.ErrSelfDelete) {
		t.Fatalf("self-delete error = %v, want ErrSelfDelete", err)
	}
	if err := repo.DeleteOtherOwner(context.Background(), first.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Authenticate(context.Background(), secondSession.Token, true); !errors.Is(err, store.ErrUnauthenticated) {
		t.Fatalf("deleted owner's session error = %v, want ErrUnauthenticated", err)
	}
	if _, err := repo.Authenticate(context.Background(), firstSession.Token, true); err != nil {
		t.Fatalf("current owner's session: %v", err)
	}

	if err := repo.ResetAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Authenticate(context.Background(), firstSession.Token, true); !errors.Is(err, store.ErrUnauthenticated) {
		t.Fatalf("session after recovery error = %v, want ErrUnauthenticated", err)
	}
	instance, err := repo.Instance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	files, err := repo.Files(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance.Claimed || instance.SiteName != "" || len(files) != 1 || files[0].Name != "kept.html" {
		t.Fatalf("after reset: instance = %#v, files = %#v", instance, files)
	}
}
