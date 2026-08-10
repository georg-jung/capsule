package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestCompressionAndConditionalRequests(t *testing.T) {
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
	file, _, err := repository.PutFile(context.Background(), "page.html", strings.NewReader("<!doctype html><h1>compressible page body</h1>"))
	if err != nil {
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

	get := func(path string, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "http://localhost:8080"+path, nil)
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		request.AddCookie(&http.Cookie{Name: "capsule_session", Value: session.Token})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}

	plain := get("/assets/app.js", nil)
	if plain.Code != http.StatusOK || plain.Header().Get("Content-Encoding") != "" {
		t.Fatalf("plain asset status = %d, encoding = %q", plain.Code, plain.Header().Get("Content-Encoding"))
	}
	if plain.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("identity response must carry Vary: headers = %v", plain.Header())
	}
	etag := plain.Header().Get("ETag")
	if etag == "" || plain.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("asset caching headers = %v", plain.Header())
	}

	compressed := get("/assets/app.js", map[string]string{"Accept-Encoding": "gzip"})
	if compressed.Code != http.StatusOK ||
		compressed.Header().Get("Content-Encoding") != "gzip" ||
		compressed.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("compressed asset status = %d, headers = %v", compressed.Code, compressed.Header())
	}
	reader, err := gzip.NewReader(compressed.Body)
	if err != nil {
		t.Fatal(err)
	}
	decompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decompressed, plain.Body.Bytes()) {
		t.Fatalf("gzip body does not match identity body: %d vs %d bytes", len(decompressed), plain.Body.Len())
	}

	declined := get("/assets/app.js", map[string]string{"Accept-Encoding": "identity;q=1, gzip;q=0"})
	if declined.Code != http.StatusOK || declined.Header().Get("Content-Encoding") != "" || declined.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("gzip;q=0 still compressed or missing Vary: headers = %v", declined.Header())
	}

	conditional := get("/assets/app.js", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": etag})
	if conditional.Code != http.StatusNotModified || conditional.Body.Len() != 0 {
		t.Fatalf("conditional asset status = %d, body length = %d", conditional.Code, conditional.Body.Len())
	}

	worker := get("/sw.js", map[string]string{"Accept-Encoding": "gzip"})
	if worker.Code != http.StatusOK || worker.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("service worker status = %d, headers = %v", worker.Code, worker.Header())
	}
	workerReader, err := gzip.NewReader(worker.Body)
	if err != nil {
		t.Fatal(err)
	}
	workerScript, err := io.ReadAll(workerReader)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(workerScript), "__CACHE_VERSION__") {
		t.Fatal("service worker cache version placeholder was not replaced")
	}
	workerConditional := get("/sw.js", map[string]string{"If-None-Match": worker.Header().Get("ETag")})
	if workerConditional.Code != http.StatusNotModified || workerConditional.Body.Len() != 0 {
		t.Fatalf("conditional sw.js status = %d, body length = %d", workerConditional.Code, workerConditional.Body.Len())
	}

	content := get("/content/"+file.ID+"/page.html", map[string]string{"Accept-Encoding": "gzip"})
	if content.Code != http.StatusOK || content.Header().Get("Content-Encoding") != "" {
		t.Fatalf("content response must stay identity-encoded: status = %d, headers = %v", content.Code, content.Header())
	}
	rangeResponse := get("/content/"+file.ID+"/page.html", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-8"})
	if rangeResponse.Code != http.StatusPartialContent ||
		rangeResponse.Header().Get("Content-Encoding") != "" ||
		rangeResponse.Body.String() != "<!doctype" {
		t.Fatalf("range status = %d, headers = %v, body = %q", rangeResponse.Code, rangeResponse.Header(), rangeResponse.Body.String())
	}
}

func TestAuthenticatedRequestsSlideSessionExpiryAfterTwentyFourHours(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	repository, err := store.Open(context.Background(), store.Config{
		DataDir: t.TempDir(),
		Now:     func() time.Time { return clock() },
	})
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

	requestLibrary := func() *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/library", nil)
		request.AddCookie(&http.Cookie{Name: "capsule_session", Value: session.Token})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}

	// Within the 24h renewal window, the session cookie must not be re-issued.
	now = now.Add(23 * time.Hour)
	early := requestLibrary()
	if early.Code != http.StatusOK {
		t.Fatalf("early library status = %d", early.Code)
	}
	if len(early.Result().Cookies()) != 0 {
		t.Fatalf("early request re-issued cookies: %v", early.Result().Cookies())
	}

	// Past the 24h renewal window, the session cookie must be re-issued with a later expiry.
	now = now.Add(2 * time.Hour) // 25h since creation
	renewed := requestLibrary()
	if renewed.Code != http.StatusOK {
		t.Fatalf("renewed library status = %d", renewed.Code)
	}
	cookies := renewed.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, cookie := range cookies {
		if cookie.Name == "capsule_session" {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil {
		t.Fatalf("expected a re-issued session cookie, got %v", cookies)
	}
	if sessionCookie.Value != session.Token {
		t.Fatalf("re-issued cookie token = %q, want unchanged %q", sessionCookie.Value, session.Token)
	}
	wantExpiry := now.Add(90 * 24 * time.Hour)
	if !sessionCookie.Expires.Equal(wantExpiry) {
		t.Fatalf("re-issued cookie expiry = %v, want %v", sessionCookie.Expires, wantExpiry)
	}
	if !sessionCookie.Expires.After(session.ExpiresAt) {
		t.Fatalf("re-issued cookie expiry %v did not move past original %v", sessionCookie.Expires, session.ExpiresAt)
	}
	// Strict would be withheld from an installed PWA's launch navigation, so a
	// cold start would show the login page despite an unexpired session.
	if sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie SameSite = %v, want Lax", sessionCookie.SameSite)
	}
	if !sessionCookie.HttpOnly {
		t.Fatal("session cookie must stay HttpOnly")
	}
}
