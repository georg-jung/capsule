package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/georg-jung/capsule/internal/store"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

var ErrCeremony = errors.New("authentication ceremony is invalid, expired, or already used")
var ErrCeremonyCapacity = errors.New("too many authentication ceremonies are pending")

const maxPendingCeremonies = 128

type Manager struct {
	webauthn *webauthn.WebAuthn
	store    *store.Store
	now      func() time.Time

	mu      sync.Mutex
	pending map[string]pendingCeremony
}

type pendingCeremony struct {
	kind        string
	user        *webUser
	siteName    string
	inviteToken string
	session     webauthn.SessionData
	expiresAt   time.Time
}

type BeginResult struct {
	CeremonyID string `json:"ceremonyId"`
	Options    any    `json:"options"`
}

func NewManager(origin, rpid string, repository *store.Store, now func() time.Time) (*Manager, error) {
	if now == nil {
		now = time.Now
	}
	instance, err := webauthn.New(&webauthn.Config{
		RPID:                  rpid,
		RPDisplayName:         "Capsule",
		RPOrigins:             []string{origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("configure WebAuthn: %w", err)
	}
	return &Manager{webauthn: instance, store: repository, now: now, pending: make(map[string]pendingCeremony)}, nil
}

func (m *Manager) BeginSetup(ctx context.Context, siteName, personName string) (BeginResult, error) {
	instance, err := m.store.Instance(ctx)
	if err != nil {
		return BeginResult{}, err
	}
	if instance.Claimed {
		return BeginResult{}, store.ErrClaimed
	}
	siteName, err = cleanName(siteName, 80, "site name")
	if err != nil {
		return BeginResult{}, err
	}
	personName, err = cleanName(personName, 100, "person name")
	if err != nil {
		return BeginResult{}, err
	}
	return m.beginRegistration("setup", siteName, "", personName)
}

func (m *Manager) BeginInvite(ctx context.Context, inviteToken, personName string) (BeginResult, error) {
	if _, err := m.store.ValidateInvite(ctx, inviteToken); err != nil {
		return BeginResult{}, err
	}
	personName, err := cleanName(personName, 100, "person name")
	if err != nil {
		return BeginResult{}, err
	}
	return m.beginRegistration("invite", "", inviteToken, personName)
}

func (m *Manager) beginRegistration(kind, siteName, inviteToken, personName string) (BeginResult, error) {
	ownerID, err := randomText(18)
	if err != nil {
		return BeginResult{}, err
	}
	userHandle := make([]byte, 32)
	if _, err := rand.Read(userHandle); err != nil {
		return BeginResult{}, fmt.Errorf("generate user handle: %w", err)
	}
	user := &webUser{record: store.Owner{ID: ownerID, UserHandle: userHandle, PersonName: personName}}
	options, session, err := m.webauthn.BeginRegistration(user,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
	)
	if err != nil {
		return BeginResult{}, fmt.Errorf("begin passkey registration: %w", err)
	}
	id, err := randomText(32)
	if err != nil {
		return BeginResult{}, err
	}
	m.mu.Lock()
	m.purgeExpiredLocked()
	if len(m.pending) >= maxPendingCeremonies {
		m.mu.Unlock()
		return BeginResult{}, ErrCeremonyCapacity
	}
	m.pending[id] = pendingCeremony{kind: kind, user: user, siteName: siteName, inviteToken: inviteToken, session: *session, expiresAt: m.now().Add(5 * time.Minute)}
	m.mu.Unlock()
	return BeginResult{CeremonyID: id, Options: options}, nil
}

func (m *Manager) FinishRegistration(ctx context.Context, ceremonyID, userAgent string, request *http.Request) (store.SessionCredentials, error) {
	pending, err := m.pop(ceremonyID, "setup", "invite")
	if err != nil {
		return store.SessionCredentials{}, err
	}
	credential, err := m.webauthn.FinishRegistration(pending.user, pending.session, request)
	if err != nil {
		return store.SessionCredentials{}, fmt.Errorf("verify passkey registration: %w", err)
	}
	owner, err := ownerFromCredential(pending.user.record, credential, userAgent)
	if err != nil {
		return store.SessionCredentials{}, err
	}
	switch pending.kind {
	case "setup":
		err = m.store.Claim(ctx, pending.siteName, owner)
	case "invite":
		err = m.store.AcceptInvite(ctx, pending.inviteToken, owner)
	default:
		err = ErrCeremony
	}
	if err != nil {
		return store.SessionCredentials{}, err
	}
	return m.store.CreateSession(ctx, owner.ID)
}

func (m *Manager) BeginLogin() (BeginResult, error) {
	options, session, err := m.webauthn.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		return BeginResult{}, fmt.Errorf("begin passkey login: %w", err)
	}
	id, err := randomText(32)
	if err != nil {
		return BeginResult{}, err
	}
	m.mu.Lock()
	m.purgeExpiredLocked()
	if len(m.pending) >= maxPendingCeremonies {
		m.mu.Unlock()
		return BeginResult{}, ErrCeremonyCapacity
	}
	m.pending[id] = pendingCeremony{kind: "login", session: *session, expiresAt: m.now().Add(5 * time.Minute)}
	m.mu.Unlock()
	return BeginResult{CeremonyID: id, Options: options}, nil
}

func (m *Manager) FinishLogin(ctx context.Context, ceremonyID string, request *http.Request) (store.SessionCredentials, error) {
	pending, err := m.pop(ceremonyID, "login")
	if err != nil {
		return store.SessionCredentials{}, err
	}
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		owner, lookupErr := m.store.OwnerByUserHandle(ctx, userHandle)
		if lookupErr != nil || !bytes.Equal(rawID, owner.CredentialID) {
			return nil, store.ErrUnauthenticated
		}
		return webUserFromOwner(owner)
	}
	user, credential, err := m.webauthn.FinishPasskeyLogin(handler, pending.session, request)
	if err != nil {
		return store.SessionCredentials{}, fmt.Errorf("verify passkey login: %w", err)
	}
	resolved, ok := user.(*webUser)
	if !ok {
		return store.SessionCredentials{}, errors.New("WebAuthn returned an unexpected user type")
	}
	updated, err := ownerFromCredential(resolved.record, credential, resolved.record.UserAgent)
	if err != nil {
		return store.SessionCredentials{}, err
	}
	updated.PasskeyName = resolved.record.PasskeyName
	if err := m.store.UpdateOwnerCredential(ctx, updated); err != nil {
		return store.SessionCredentials{}, err
	}
	return m.store.CreateSession(ctx, updated.ID)
}

func (m *Manager) pop(id string, kinds ...string) (pendingCeremony, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pending, found := m.pending[id]
	delete(m.pending, id)
	if !found || !m.now().Before(pending.expiresAt) {
		return pendingCeremony{}, ErrCeremony
	}
	for _, kind := range kinds {
		if pending.kind == kind {
			return pending, nil
		}
	}
	return pendingCeremony{}, ErrCeremony
}

func (m *Manager) purgeExpiredLocked() {
	now := m.now()
	for id, pending := range m.pending {
		if !now.Before(pending.expiresAt) {
			delete(m.pending, id)
		}
	}
}

type webUser struct {
	record      store.Owner
	credentials []webauthn.Credential
}

func (u *webUser) WebAuthnID() []byte                         { return u.record.UserHandle }
func (u *webUser) WebAuthnName() string                       { return u.record.PersonName }
func (u *webUser) WebAuthnDisplayName() string                { return u.record.PersonName }
func (u *webUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

func webUserFromOwner(owner store.Owner) (*webUser, error) {
	var credential webauthn.Credential
	if err := json.Unmarshal(owner.CredentialJSON, &credential); err != nil {
		return nil, fmt.Errorf("decode stored credential: %w", err)
	}
	return &webUser{record: owner, credentials: []webauthn.Credential{credential}}, nil
}

func ownerFromCredential(owner store.Owner, credential *webauthn.Credential, userAgent string) (store.Owner, error) {
	encoded, err := json.Marshal(credential)
	if err != nil {
		return store.Owner{}, fmt.Errorf("encode credential: %w", err)
	}
	transports := make([]string, len(credential.Transport))
	for index, transport := range credential.Transport {
		transports[index] = string(transport)
	}
	owner.CredentialID = credential.ID
	owner.CredentialJSON = encoded
	owner.PasskeyName = PasskeyName(credential)
	owner.AAGUID = FormatAAGUID(credential.Authenticator.AAGUID)
	owner.Attachment = string(credential.Authenticator.Attachment)
	owner.Transports = transports
	owner.BackupEligible = credential.Flags.BackupEligible
	owner.BackupState = credential.Flags.BackupState
	owner.UserAgent = userAgent
	owner.SignCount = credential.Authenticator.SignCount
	owner.CloneWarning = credential.Authenticator.CloneWarning
	return owner, nil
}

func cleanName(value string, maxRunes int, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len([]rune(value)) > maxRunes {
		return "", fmt.Errorf("%s must contain between 1 and %d characters", field, maxRunes)
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return "", fmt.Errorf("%s must not contain control characters", field)
		}
	}
	return value, nil
}

func randomText(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
