package app

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/georg-jung/capsule/internal/store"
)

func (s *Server) handleSetupBegin(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		SiteName   string `json:"siteName"`
		PersonName string `json:"personName"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	result, err := s.authenticator.BeginSetup(request.Context(), input.SiteName, input.PersonName)
	if errors.Is(err, store.ErrClaimed) {
		writeProblem(writer, http.StatusConflict, "This instance has already been claimed.")
		return
	}
	if err != nil {
		slog.Error("passkey setup could not begin", "error", err)
		writeProblem(writer, http.StatusBadRequest, err.Error())
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) handleRegistrationFinish(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	session, err := s.authenticator.FinishRegistration(request.Context(), request.Header.Get("X-Ceremony-ID"), request.UserAgent(), request)
	if err != nil {
		slog.Error("passkey registration failed", "error", err)
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrClaimed) || errors.Is(err, store.ErrInvalidInvite) {
			status = http.StatusConflict
		}
		writeProblem(writer, status, "Passkey registration could not be completed.")
		return
	}
	s.setSessionCookie(writer, session)
	clearCookie(writer, s.joinCookie, s.secureCookies)
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLoginBegin(writer http.ResponseWriter, _ *http.Request) {
	result, err := s.authenticator.BeginLogin()
	if err != nil {
		writeProblem(writer, http.StatusBadRequest, "Passkey authentication could not be started.")
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) handleLoginFinish(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	session, err := s.authenticator.FinishLogin(request.Context(), request.Header.Get("X-Ceremony-ID"), request)
	if err != nil {
		slog.Error("passkey login failed", "error", err)
		writeProblem(writer, http.StatusUnauthorized, "Passkey authentication failed.")
		return
	}
	s.setSessionCookie(writer, session)
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleInviteExchange(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Token string `json:"token"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	invite, err := s.store.ValidateInvite(request.Context(), input.Token)
	if err != nil {
		writeProblem(writer, http.StatusGone, "This invite is invalid, expired, or already used.")
		return
	}
	http.SetCookie(writer, &http.Cookie{
		Name:     s.joinCookie,
		Value:    invite.Token,
		Path:     "/",
		Secure:   s.secureCookies,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Expires:  invite.ExpiresAt,
	})
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{"expiresAt": invite.ExpiresAt})
}

func (s *Server) handleInviteBegin(writer http.ResponseWriter, request *http.Request) {
	invite, err := request.Cookie(s.joinCookie)
	if err != nil || invite.Value == "" {
		writeProblem(writer, http.StatusGone, "This invite is invalid, expired, or already used.")
		return
	}
	var input struct {
		PersonName string `json:"personName"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	result, err := s.authenticator.BeginInvite(request.Context(), invite.Value, input.PersonName)
	if err != nil {
		writeProblem(writer, http.StatusGone, "This invite is invalid, expired, or already used.")
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) handleLogout(writer http.ResponseWriter, request *http.Request) {
	if cookie, err := request.Cookie(s.sessionCookie); err == nil {
		_ = s.store.DeleteSession(request.Context(), cookie.Value)
	}
	clearCookie(writer, s.sessionCookie, s.secureCookies)
	writer.Header().Set("Clear-Site-Data", `"cache"`)
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) setSessionCookie(writer http.ResponseWriter, session store.SessionCredentials) {
	http.SetCookie(writer, &http.Cookie{
		Name:     s.sessionCookie,
		Value:    session.Token,
		Path:     "/",
		Secure:   s.secureCookies,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Expires:  session.ExpiresAt,
	})
}

func clearCookie(writer http.ResponseWriter, name string, secure bool) {
	http.SetCookie(writer, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Unix(1, 0),
		MaxAge:   -1,
	})
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeProblem(writer, http.StatusBadRequest, "Request body is invalid.")
		return false
	}
	return true
}
