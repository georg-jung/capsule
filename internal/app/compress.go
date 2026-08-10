package app

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

var compressibleTypes = []string{
	"text/html",
	"text/css",
	"text/javascript",
	"text/plain",
	"application/json",
	"application/manifest+json",
	"image/svg+xml",
}

type compressingWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	decided     bool
	compressing bool
}

func (w *compressingWriter) decide(status int) {
	w.decided = true
	if status < http.StatusOK || status >= http.StatusMultipleChoices || status == http.StatusPartialContent {
		return
	}
	header := w.Header()
	if header.Get("Content-Encoding") != "" {
		return
	}
	contentType := header.Get("Content-Type")
	compressible := false
	for _, candidate := range compressibleTypes {
		if strings.HasPrefix(contentType, candidate) {
			compressible = true
			break
		}
	}
	if !compressible {
		return
	}
	header.Del("Content-Length")
	header.Set("Content-Encoding", "gzip")
	w.compressing = true
	w.gz = gzip.NewWriter(w.ResponseWriter)
}

func (w *compressingWriter) WriteHeader(status int) {
	if !w.decided {
		w.decide(status)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *compressingWriter) Write(data []byte) (int, error) {
	if !w.decided {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", http.DetectContentType(data))
		}
		w.decide(http.StatusOK)
	}
	if w.compressing {
		return w.gz.Write(data)
	}
	return w.ResponseWriter.Write(data)
}

func (w *compressingWriter) Flush() {
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *compressingWriter) close() {
	if w.gz != nil {
		_ = w.gz.Close()
	}
}

// acceptsGzip reports whether the Accept-Encoding header allows gzip,
// honoring quality values: an explicit gzip entry wins over a wildcard, and
// q=0 on either declares the coding unacceptable.
func acceptsGzip(header string) bool {
	gzipQuality, wildcardQuality := -1.0, -1.0
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(part, ";")
		name := strings.ToLower(strings.TrimSpace(fields[0]))
		if name != "gzip" && name != "*" {
			continue
		}
		quality := 1.0
		for _, param := range fields[1:] {
			param = strings.ToLower(strings.ReplaceAll(param, " ", ""))
			if value, ok := strings.CutPrefix(param, "q="); ok {
				if parsed, err := strconv.ParseFloat(value, 64); err == nil {
					quality = parsed
				}
			}
		}
		if name == "*" {
			wildcardQuality = quality
		} else {
			gzipQuality = quality
		}
	}
	if gzipQuality >= 0 {
		return gzipQuality > 0
	}
	return wildcardQuality > 0
}

func (s *Server) compressResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || strings.HasPrefix(request.URL.Path, "/content/") {
			next.ServeHTTP(writer, request)
			return
		}
		// Vary applies to the identity representation too, so caches never
		// reuse it for a request that negotiated a different encoding.
		writer.Header().Add("Vary", "Accept-Encoding")
		if !acceptsGzip(request.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(writer, request)
			return
		}
		compressed := &compressingWriter{ResponseWriter: writer}
		defer compressed.close()
		next.ServeHTTP(compressed, request)
	})
}
