package app

import (
	"context"
	"crypto/subtle"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/georg-jung/capsule/internal/auth"
	"github.com/georg-jung/capsule/internal/store"
)

//go:embed web/templates/*.html web/assets/*
var webFiles embed.FS

type Server struct {
	config         Config
	store          *store.Store
	authenticator  *auth.Manager
	templates      *template.Template
	handler        http.Handler
	sessionCookie  string
	joinCookie     string
	secureCookies  bool
	authBeginLimit *tokenBucket
	swScript       []byte
	swETag         string
	assetETags     map[string]string
}

type pageData struct {
	SiteName string
}

type contextKey string

const authenticatedKey contextKey = "authenticated"

func NewServer(config Config, repository *store.Store, authenticator *auth.Manager) (*Server, error) {
	templates, err := template.ParseFS(webFiles, "web/templates/*.html")
	if err != nil {
		return nil, err
	}
	server := &Server{
		config:         config,
		store:          repository,
		authenticator:  authenticator,
		templates:      templates,
		sessionCookie:  "capsule_session",
		joinCookie:     "capsule_join",
		authBeginLimit: newTokenBucket(30, time.Minute, nil),
	}
	if err := server.prepareStaticAssets(); err != nil {
		return nil, err
	}
	if parsed, parseErr := url.Parse(config.Origin); parseErr == nil && parsed.Scheme == "https" {
		server.secureCookies = true
		server.sessionCookie = "__Host-capsule_session"
		server.joinCookie = "__Host-capsule_join"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", server.handleLanding)
	mux.HandleFunc("GET /join", server.handleJoin)
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /sw.js", server.handleServiceWorker)
	assets, err := fs.Sub(webFiles, "web/assets")
	if err != nil {
		return nil, err
	}
	mux.Handle("GET /assets/", server.assetCaching(http.StripPrefix("/assets/", http.FileServerFS(assets))))
	mux.Handle("GET /app", server.requireAuthentication(http.HandlerFunc(server.handleApp), true))
	mux.Handle("GET /manifest.webmanifest", server.requireAuthentication(http.HandlerFunc(server.handleManifest), false))
	mux.Handle("POST /auth/setup/begin", server.requirePublicOrigin(server.limitAuthenticationBegin(http.HandlerFunc(server.handleSetupBegin))))
	mux.Handle("POST /auth/setup/finish", server.requirePublicOrigin(http.HandlerFunc(server.handleRegistrationFinish)))
	mux.Handle("POST /auth/login/begin", server.requirePublicOrigin(server.limitAuthenticationBegin(http.HandlerFunc(server.handleLoginBegin))))
	mux.Handle("POST /auth/login/finish", server.requirePublicOrigin(http.HandlerFunc(server.handleLoginFinish)))
	mux.Handle("POST /auth/invite/exchange", server.requirePublicOrigin(http.HandlerFunc(server.handleInviteExchange)))
	mux.Handle("POST /auth/invite/begin", server.requirePublicOrigin(server.limitAuthenticationBegin(http.HandlerFunc(server.handleInviteBegin))))
	mux.Handle("POST /auth/invite/finish", server.requirePublicOrigin(http.HandlerFunc(server.handleRegistrationFinish)))
	mux.Handle("POST /auth/logout", server.requireAuthentication(server.requireMutation(http.HandlerFunc(server.handleLogout)), false))
	mux.Handle("GET /api/library", server.requireAuthentication(http.HandlerFunc(server.handleLibrary), false))
	mux.Handle("GET /api/admin", server.requireAuthentication(http.HandlerFunc(server.handleAdmin), false))
	mux.Handle("POST /api/files", server.requireAuthentication(server.requireMutation(http.HandlerFunc(server.handleUpload)), false))
	mux.Handle("POST /api/files/{id}/rename", server.requireAuthentication(server.requireMutation(http.HandlerFunc(server.handleFileRename)), false))
	mux.Handle("DELETE /api/files/{id}", server.requireAuthentication(server.requireMutation(http.HandlerFunc(server.handleFileDelete)), false))
	mux.Handle("POST /api/invites", server.requireAuthentication(server.requireMutation(http.HandlerFunc(server.handleCreateInvite)), false))
	mux.Handle("POST /api/site", server.requireAuthentication(server.requireMutation(http.HandlerFunc(server.handleSiteRename)), false))
	mux.Handle("POST /api/owners/{id}/rename", server.requireAuthentication(server.requireMutation(http.HandlerFunc(server.handlePasskeyRename)), false))
	mux.Handle("DELETE /api/owners/{id}", server.requireAuthentication(server.requireMutation(http.HandlerFunc(server.handleOwnerDelete)), false))
	mux.Handle("GET /content/{id}/{name}", server.requireAuthentication(http.HandlerFunc(server.handleContent), false))
	server.handler = server.securityHeaders(server.compressResponses(mux))
	return server, nil
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	s.handler.ServeHTTP(writer, request)
}

func (s *Server) handleLanding(writer http.ResponseWriter, request *http.Request) {
	instance, err := s.store.Instance(request.Context())
	if err != nil {
		http.Error(writer, "The private app is temporarily unavailable.", http.StatusInternalServerError)
		return
	}
	if !instance.Claimed {
		s.render(writer, "setup.html", pageData{})
		return
	}
	if authenticated, token, err := s.session(request); err == nil {
		if authenticated.Extended {
			s.setSessionCookie(writer, store.SessionCredentials{Token: token, ExpiresAt: authenticated.ExpiresAt})
		}
		http.Redirect(writer, request, "/app", http.StatusSeeOther)
		return
	}
	s.render(writer, "login.html", pageData{})
}

func (s *Server) handleApp(writer http.ResponseWriter, request *http.Request) {
	instance, err := s.store.Instance(request.Context())
	if err != nil {
		http.Error(writer, "The private app is temporarily unavailable.", http.StatusInternalServerError)
		return
	}
	s.render(writer, "app.html", pageData{SiteName: instance.SiteName})
}

func (s *Server) handleJoin(writer http.ResponseWriter, _ *http.Request) {
	s.render(writer, "join.html", pageData{})
}

func (s *Server) render(writer http.ResponseWriter, name string, data pageData) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	if err := s.templates.ExecuteTemplate(writer, name, data); err != nil {
		http.Error(writer, "The private app is temporarily unavailable.", http.StatusInternalServerError)
	}
}

// session authenticates the request's session cookie and also returns the
// raw cookie token so callers can re-issue the cookie when the session was
// extended.
func (s *Server) session(request *http.Request) (store.Authenticated, string, error) {
	cookie, err := request.Cookie(s.sessionCookie)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return store.Authenticated{}, "", store.ErrUnauthenticated
	}
	authenticated, err := s.store.Authenticate(request.Context(), cookie.Value)
	if err != nil {
		return store.Authenticated{}, "", err
	}
	return authenticated, cookie.Value, nil
}

func (s *Server) requireAuthentication(next http.Handler, browserPage bool) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authenticated, token, err := s.session(request)
		if err != nil {
			if browserPage {
				http.Redirect(writer, request, "/", http.StatusSeeOther)
			} else {
				http.Error(writer, "authentication required", http.StatusUnauthorized)
			}
			return
		}
		if authenticated.Extended {
			s.setSessionCookie(writer, store.SessionCredentials{Token: token, ExpiresAt: authenticated.ExpiresAt})
		}
		ctx := context.WithValue(request.Context(), authenticatedKey, authenticated)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func (s *Server) requireMutation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authenticated, ok := request.Context().Value(authenticatedKey).(store.Authenticated)
		if !ok || request.Header.Get("Origin") != s.config.Origin || subtle.ConstantTimeCompare(
			[]byte(request.Header.Get("X-CSRF-Token")), []byte(authenticated.CSRFToken),
		) != 1 {
			http.Error(writer, "request origin or CSRF token is invalid", http.StatusForbidden)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) requirePublicOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Origin") != s.config.Origin {
			http.Error(writer, "request origin is invalid", http.StatusForbidden)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) limitAuthenticationBegin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !s.authBeginLimit.allow() {
			writer.Header().Set("Retry-After", "60")
			writeProblem(writer, http.StatusTooManyRequests, "Too many passkey attempts. Try again shortly.")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		if !strings.HasPrefix(request.URL.Path, "/content/") {
			writer.Header().Set("X-Frame-Options", "DENY")
			writer.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
			writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		}
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) notImplemented(writer http.ResponseWriter, _ *http.Request) {
	http.Error(writer, "not implemented", http.StatusNotImplemented)
}
