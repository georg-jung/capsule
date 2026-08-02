package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/georg-jung/capsule/internal/store"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

func TestPasskeyNameUsesAAGUIDWithSafeFallbacks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		aaguid     string
		attachment protocol.AuthenticatorAttachment
		backup     bool
		want       string
	}{
		{name: "Google Password Manager", aaguid: "ea9b8d664d011d213ce4b6b48cb575d4", want: "Google Password Manager"},
		{name: "Windows Hello hardware", aaguid: "08987058cadc4b81b6e130de50dcbe96", want: "Windows Hello"},
		{name: "Windows Hello VBS", aaguid: "9ddd1817af5a4672a2b93e3dd95000a9", want: "Windows Hello"},
		{name: "synced platform fallback", aaguid: "11111111111111111111111111111111", attachment: protocol.Platform, backup: true, want: "Synced platform passkey"},
		{name: "platform fallback", attachment: protocol.Platform, want: "Platform passkey"},
		{name: "generic fallback", want: "Passkey"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			aaguid, err := hex.DecodeString(test.aaguid)
			if err != nil {
				t.Fatal(err)
			}
			credential := &webauthn.Credential{
				Authenticator: webauthn.Authenticator{AAGUID: aaguid, Attachment: test.attachment},
				Flags:         webauthn.CredentialFlags{BackupEligible: test.backup},
			}
			if got := PasskeyName(credential); got != test.want {
				t.Fatalf("PasskeyName() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPendingCeremoniesAreBoundedAndExpiredEntriesArePurged(t *testing.T) {
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	repository, err := store.Open(context.Background(), store.Config{
		DataDir: t.TempDir(),
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	manager, err := NewManager("http://localhost:8080", "localhost", repository, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}

	for range maxPendingCeremonies {
		if _, err := manager.BeginLogin(); err != nil {
			t.Fatalf("fill pending ceremonies: %v", err)
		}
	}
	if _, err := manager.BeginLogin(); !errors.Is(err, ErrCeremonyCapacity) {
		t.Fatalf("capacity error = %v, want ErrCeremonyCapacity", err)
	}
	now = now.Add(5 * time.Minute)
	if _, err := manager.BeginLogin(); err != nil {
		t.Fatalf("begin after expiry purge: %v", err)
	}
}
