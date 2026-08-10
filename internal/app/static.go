package app

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
)

// prepareStaticAssets computes strong ETags for every embedded asset and a
// combined content version that is injected into the service worker script so
// its shell cache name changes whenever any asset changes.
func (s *Server) prepareStaticAssets() error {
	entries, err := fs.ReadDir(webFiles, "web/assets")
	if err != nil {
		return err
	}
	overall := sha256.New()
	s.assetETags = make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, err := webFiles.ReadFile("web/assets/" + entry.Name())
		if err != nil {
			return err
		}
		digest := sha256.Sum256(content)
		s.assetETags[entry.Name()] = `"` + hex.EncodeToString(digest[:16]) + `"`
		overall.Write([]byte(entry.Name()))
		overall.Write(digest[:])
	}
	version := hex.EncodeToString(overall.Sum(nil))[:12]
	script, err := webFiles.ReadFile("web/assets/sw.js")
	if err != nil {
		return err
	}
	s.swScript = []byte(strings.Replace(string(script), "__CACHE_VERSION__", version, 1))
	digest := sha256.Sum256(s.swScript)
	s.swETag = `"` + hex.EncodeToString(digest[:16]) + `"`
	return nil
}

func (s *Server) assetCaching(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if etag, ok := s.assetETags[strings.TrimPrefix(request.URL.Path, "/assets/")]; ok {
			writer.Header().Set("ETag", etag)
			writer.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(writer, request)
	})
}

func requestETagMatches(request *http.Request, etag string) bool {
	for _, candidate := range strings.Split(request.Header.Get("If-None-Match"), ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate != "" && (candidate == etag || candidate == "*") {
			return true
		}
	}
	return false
}
