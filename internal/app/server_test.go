package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/georg-jung/capsule/internal/auth"
	"github.com/georg-jung/capsule/internal/store"
)

func TestAnonymousSurfaceDoesNotLeakPrivateInstanceData(t *testing.T) {
	t.Parallel()

	repository, err := store.Open(context.Background(), store.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	owner := store.Owner{
		ID:             "owner-one",
		UserHandle:     []byte("owner-one-handle"),
		CredentialID:   []byte("credential-one"),
		PersonName:     "Georg",
		PasskeyName:    "Windows Hello",
		CredentialJSON: []byte(`{"id":"credential-one"}`),
	}
	if err := repository.Claim(context.Background(), "Secret tools", owner); err != nil {
		t.Fatal(err)
	}
	file, _, err := repository.PutFile(context.Background(), "secret.html", strings.NewReader("classified bytes"))
	if err != nil {
		t.Fatal(err)
	}

	config := Config{Origin: "http://localhost:8080", RPID: "localhost", MaxUploadSize: 1 << 20}
	authenticator, err := auth.NewManager(config.Origin, config.RPID, repository, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(config, repository, authenticator)
	if err != nil {
		t.Fatal(err)
	}

	public := httptest.NewRecorder()
	server.ServeHTTP(public, httptest.NewRequest(http.MethodGet, "http://localhost:8080/", nil))
	if public.Code != http.StatusOK {
		t.Fatalf("landing status = %d", public.Code)
	}
	landing := public.Body.String()
	for _, secret := range []string{"Secret tools", "secret.html", "classified bytes", "Georg"} {
		if strings.Contains(landing, secret) {
			t.Fatalf("landing leaked %q: %s", secret, landing)
		}
	}
	if !strings.Contains(landing, "Private app") {
		t.Fatalf("landing = %s", landing)
	}

	privatePaths := []string{
		"/app",
		"/api/library",
		"/api/admin",
		"/content/" + file.ID + "/secret.html",
	}
	for _, path := range privatePaths {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "http://localhost:8080"+path, nil)
		server.ServeHTTP(response, request)
		body, _ := io.ReadAll(response.Result().Body)
		if response.Code != http.StatusUnauthorized && response.Code != http.StatusSeeOther {
			t.Fatalf("GET %s status = %d, body = %s", path, response.Code, body)
		}
		if strings.Contains(string(body), "classified bytes") || strings.Contains(string(body), "secret.html") {
			t.Fatalf("GET %s leaked private data: %s", path, body)
		}
	}
}

func TestAuthenticatedUploadRequiresOriginAndCSRFAndServesCompleteFiles(t *testing.T) {
	t.Parallel()

	repository, err := store.Open(context.Background(), store.Config{DataDir: t.TempDir(), MaxUploadSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	owner := store.Owner{
		ID:             "owner-one",
		UserHandle:     []byte("owner-one-handle"),
		CredentialID:   []byte("credential-one"),
		PersonName:     "Georg",
		PasskeyName:    "Windows Hello",
		CredentialJSON: []byte(`{"id":"credential-one"}`),
	}
	if err := repository.Claim(context.Background(), "Secret tools", owner); err != nil {
		t.Fatal(err)
	}
	session, err := repository.CreateSession(context.Background(), owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Origin: "http://localhost:8080", RPID: "localhost", MaxUploadSize: 1 << 20}
	authenticator, err := auth.NewManager(config.Origin, config.RPID, repository, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(config, repository, authenticator)
	if err != nil {
		t.Fatal(err)
	}

	requestUpload := func(origin, csrf string, files map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for name, contents := range files {
			part, partErr := writer.CreateFormFile("files", name)
			if partErr != nil {
				t.Fatal(partErr)
			}
			if _, partErr = io.WriteString(part, contents); partErr != nil {
				t.Fatal(partErr)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/files", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.Header.Set("Origin", origin)
		request.Header.Set("X-CSRF-Token", csrf)
		request.AddCookie(&http.Cookie{Name: "capsule_session", Value: session.Token})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}

	if response := requestUpload("https://evil.example", session.CSRFToken, map[string]string{"leak.html": "no"}); response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin upload status = %d, body = %s", response.Code, response.Body.String())
	}
	if response := requestUpload(config.Origin, "", map[string]string{"leak.html": "no"}); response.Code != http.StatusForbidden {
		t.Fatalf("missing-CSRF upload status = %d, body = %s", response.Code, response.Body.String())
	}
	if response := requestUpload(config.Origin, session.CSRFToken, map[string]string{
		"first.html":  "first complete file",
		"second.html": "second complete file",
	}); response.Code != http.StatusOK {
		t.Fatalf("valid upload status = %d, body = %s", response.Code, response.Body.String())
	}

	libraryRequest := httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/library", nil)
	libraryRequest.AddCookie(&http.Cookie{Name: "capsule_session", Value: session.Token})
	libraryResponse := httptest.NewRecorder()
	server.ServeHTTP(libraryResponse, libraryRequest)
	var library struct {
		SiteName string       `json:"siteName"`
		Files    []store.File `json:"files"`
	}
	if err := json.Unmarshal(libraryResponse.Body.Bytes(), &library); err != nil {
		t.Fatalf("decode library: %v; body = %s", err, libraryResponse.Body.String())
	}
	if library.SiteName != "Secret tools" || len(library.Files) != 2 {
		t.Fatalf("library = %#v", library)
	}

	contentRequest := httptest.NewRequest(http.MethodGet, "http://localhost:8080/content/"+library.Files[0].ID+"/"+library.Files[0].Name, nil)
	contentRequest.Header.Set("Range", "bytes=0-4")
	contentRequest.AddCookie(&http.Cookie{Name: "capsule_session", Value: session.Token})
	contentResponse := httptest.NewRecorder()
	server.ServeHTTP(contentResponse, contentRequest)
	if contentResponse.Code != http.StatusPartialContent || contentResponse.Body.String() != "first" {
		t.Fatalf("range status = %d, body = %q", contentResponse.Code, contentResponse.Body.String())
	}
}
