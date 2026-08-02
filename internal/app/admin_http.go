package app

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/georg-jung/capsule/internal/store"
)

type ownerView struct {
	ID             string     `json:"id"`
	PersonName     string     `json:"personName"`
	PasskeyName    string     `json:"passkeyName"`
	CredentialID   string     `json:"credentialId"`
	AAGUID         string     `json:"aaguid"`
	Attachment     string     `json:"attachment"`
	Transports     []string   `json:"transports"`
	BackupEligible bool       `json:"backupEligible"`
	BackupState    bool       `json:"backupState"`
	SignCount      uint32     `json:"signCount"`
	CloneWarning   bool       `json:"cloneWarning"`
	UserAgent      string     `json:"userAgent"`
	CreatedAt      time.Time  `json:"createdAt"`
	LastUsedAt     *time.Time `json:"lastUsedAt,omitempty"`
	Current        bool       `json:"current"`
}

func (s *Server) handleAdmin(writer http.ResponseWriter, request *http.Request) {
	authenticated := request.Context().Value(authenticatedKey).(store.Authenticated)
	owners, err := s.store.Owners(request.Context())
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "Could not load owners.")
		return
	}
	views := make([]ownerView, 0, len(owners))
	for _, owner := range owners {
		views = append(views, ownerView{
			ID: owner.ID, PersonName: owner.PersonName, PasskeyName: owner.PasskeyName,
			CredentialID: base64.RawURLEncoding.EncodeToString(owner.CredentialID), AAGUID: owner.AAGUID,
			Attachment: owner.Attachment, Transports: owner.Transports, BackupEligible: owner.BackupEligible,
			BackupState: owner.BackupState, SignCount: owner.SignCount, CloneWarning: owner.CloneWarning,
			UserAgent: owner.UserAgent, CreatedAt: owner.CreatedAt, LastUsedAt: owner.LastUsedAt,
			Current: owner.ID == authenticated.Owner.ID,
		})
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{"owners": views, "csrfToken": authenticated.CSRFToken})
}

func (s *Server) handleCreateInvite(writer http.ResponseWriter, request *http.Request) {
	authenticated := request.Context().Value(authenticatedKey).(store.Authenticated)
	invite, err := s.store.CreateInvite(request.Context(), authenticated.Owner.ID)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "Could not create an invite.")
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{
		"url": s.config.Origin + "/join#" + invite.Token, "expiresAt": invite.ExpiresAt,
	})
}

func (s *Server) handleSiteRename(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	name := strings.TrimSpace(input.Name)
	if len([]rune(name)) < 1 || len([]rune(name)) > 80 {
		writeProblem(writer, http.StatusBadRequest, "App name must contain between 1 and 80 characters.")
		return
	}
	if err := s.store.UpdateSiteName(request.Context(), name); err != nil {
		writeProblem(writer, http.StatusInternalServerError, "Could not rename this app.")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePasskeyRename(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	name := strings.TrimSpace(input.Name)
	if len([]rune(name)) < 1 || len([]rune(name)) > 100 {
		writeProblem(writer, http.StatusBadRequest, "Passkey name must contain between 1 and 100 characters.")
		return
	}
	authenticated := request.Context().Value(authenticatedKey).(store.Authenticated)
	if err := s.store.RenamePasskey(request.Context(), authenticated.Owner.ID, request.PathValue("id"), name); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeProblem(writer, status, "Could not rename this passkey.")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleOwnerDelete(writer http.ResponseWriter, request *http.Request) {
	authenticated := request.Context().Value(authenticatedKey).(store.Authenticated)
	err := s.store.DeleteOtherOwner(request.Context(), authenticated.Owner.ID, request.PathValue("id"))
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, store.ErrSelfDelete):
			status = http.StatusConflict
		case errors.Is(err, store.ErrNotFound):
			status = http.StatusNotFound
		}
		writeProblem(writer, status, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleFileRename(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	file, err := s.store.RenameFile(request.Context(), request.PathValue("id"), input.Name)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, store.ErrConflict) {
			status = http.StatusConflict
		}
		writeProblem(writer, status, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, file)
}

func (s *Server) handleFileDelete(writer http.ResponseWriter, request *http.Request) {
	if err := s.store.DeleteFile(request.Context(), request.PathValue("id")); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeProblem(writer, status, "Could not delete this file.")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}
