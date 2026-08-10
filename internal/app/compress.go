package app

import (
	"compress/gzip"
	"net/http"
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

func (s *Server) compressResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet ||
			strings.HasPrefix(request.URL.Path, "/content/") ||
			!strings.Contains(request.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(writer, request)
			return
		}
		writer.Header().Add("Vary", "Accept-Encoding")
		compressed := &compressingWriter{ResponseWriter: writer}
		defer compressed.close()
		next.ServeHTTP(compressed, request)
	})
}
