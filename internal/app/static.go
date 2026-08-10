package app

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/georg-jung/capsule/internal/store"
)

// The asset set is embedded and fixed, so every media type is spelled out
// here: an unknown extension fails startup rather than reaching a browser as
// application/octet-stream.
var assetContentTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".png":   "image/png",
	".svg":   "image/svg+xml",
	".webp":  "image/webp",
	".woff2": "font/woff2",
}

// staticAsset is an embedded file prepared once at startup: its entity tag and
// both representations the server will ever hand out. Compressing here rather
// than per request costs a few hundred kilobytes of memory and buys the best
// gzip level for free, since no request pays for it.
type staticAsset struct {
	contentType string
	etag        string
	identity    []byte
	compressed  []byte
}

func newStaticAsset(name string, content []byte) (staticAsset, error) {
	contentType, known := assetContentTypes[strings.ToLower(path.Ext(name))]
	if !known {
		return staticAsset{}, fmt.Errorf("no content type is configured for asset %q", name)
	}
	digest := sha256.Sum256(content)
	asset := staticAsset{
		contentType: contentType,
		etag:        `"` + hex.EncodeToString(digest[:16]) + `"`,
		identity:    content,
	}
	if !store.CompressibleContentType(contentType) {
		return asset, nil
	}
	compressed, err := gzipBytes(content)
	if err != nil {
		return staticAsset{}, fmt.Errorf("compress asset %q: %w", name, err)
	}
	if len(compressed) < len(content) {
		asset.compressed = compressed
	}
	return asset, nil
}

func gzipBytes(content []byte) ([]byte, error) {
	var buffer bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buffer, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(content); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// prepareStaticAssets precompresses every embedded asset, computes its strong
// ETag, and derives a combined content version that is injected into the
// service worker script so its shell cache name changes whenever any asset
// changes.
func (s *Server) prepareStaticAssets() error {
	entries, err := fs.ReadDir(webFiles, "web/assets")
	if err != nil {
		return err
	}
	overall := sha256.New()
	s.assets = make(map[string]staticAsset, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, err := webFiles.ReadFile("web/assets/" + entry.Name())
		if err != nil {
			return err
		}
		digest := sha256.Sum256(content)
		overall.Write([]byte(entry.Name()))
		overall.Write(digest[:])
		// The service worker is served from /sw.js with its cache version
		// substituted; the embedded copy still carries the placeholder and
		// must never be reachable under /assets/.
		if entry.Name() == "sw.js" {
			continue
		}
		asset, err := newStaticAsset(entry.Name(), content)
		if err != nil {
			return err
		}
		s.assets[entry.Name()] = asset
	}

	version := hex.EncodeToString(overall.Sum(nil))[:12]
	script, err := webFiles.ReadFile("web/assets/sw.js")
	if err != nil {
		return err
	}
	s.swAsset, err = newStaticAsset("sw.js", []byte(strings.Replace(string(script), "__CACHE_VERSION__", version, 1)))
	return err
}

func (s *Server) handleAsset(writer http.ResponseWriter, request *http.Request) {
	asset, found := s.assets[request.PathValue("name")]
	if !found {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Cache-Control", "no-cache")
	writeStaticAsset(writer, request, asset)
}

func writeStaticAsset(writer http.ResponseWriter, request *http.Request, asset staticAsset) {
	header := writer.Header()
	header.Set("Content-Type", asset.contentType)
	header.Set("ETag", asset.etag)
	if asset.compressed != nil {
		header.Add("Vary", "Accept-Encoding")
	}
	if requestETagMatches(request, asset.etag) {
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	body := asset.identity
	if asset.compressed != nil && acceptsGzip(request.Header.Get("Accept-Encoding")) {
		header.Set("Content-Encoding", "gzip")
		body = asset.compressed
	}
	header.Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = writer.Write(body)
}

func requestETagMatches(request *http.Request, etag string) bool {
	return etagListMatches(request.Header.Get("If-None-Match"), etag)
}

func etagListMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate != "" && (candidate == etag || candidate == "*") {
			return true
		}
	}
	return false
}
