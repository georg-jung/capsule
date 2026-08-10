package app

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/georg-jung/capsule/internal/store"
)

func (s *Server) handleLibrary(writer http.ResponseWriter, request *http.Request) {
	authenticated := request.Context().Value(authenticatedKey).(store.Authenticated)
	instance, err := s.store.Instance(request.Context())
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "Could not load this app.")
		return
	}
	files, err := s.store.Files(request.Context())
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "Could not load the file library.")
		return
	}
	writer.Header().Set("Cache-Control", "private, no-cache")
	writeJSON(writer, http.StatusOK, map[string]any{
		"siteName": instance.SiteName, "files": files, "csrfToken": authenticated.CSRFToken,
	})
}

type uploadResult struct {
	File     *store.File `json:"file,omitempty"`
	Name     string      `json:"name"`
	Replaced bool        `json:"replaced,omitempty"`
	Error    string      `json:"error,omitempty"`
}

func (s *Server) handleUpload(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, s.config.MaxUploadSize*50+(1<<20))
	reader, err := request.MultipartReader()
	if err != nil {
		writeProblem(writer, http.StatusBadRequest, "Expected a multipart file upload.")
		return
	}

	results := make([]uploadResult, 0)
	failed := 0
	for len(results) < 50 {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			writeProblem(writer, http.StatusBadRequest, "Could not read the complete upload.")
			return
		}
		if part.FormName() != "files" || part.FileName() == "" {
			_, _ = io.Copy(io.Discard, part)
			_ = part.Close()
			continue
		}
		name := part.FileName()
		file, replaced, putErr := s.store.PutFile(request.Context(), name, part)
		_ = part.Close()
		result := uploadResult{Name: name, Replaced: replaced}
		if putErr != nil {
			failed++
			if errors.Is(putErr, store.ErrFileTooLarge) {
				result.Error = "File exceeds the configured upload limit."
			} else {
				result.Error = putErr.Error()
			}
		} else {
			result.File = &file
		}
		results = append(results, result)
	}
	if len(results) == 0 {
		writeProblem(writer, http.StatusBadRequest, "No files were included.")
		return
	}
	status := http.StatusOK
	if failed != 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(writer, status, map[string]any{"results": results})
}

func (s *Server) handleContent(writer http.ResponseWriter, request *http.Request) {
	// A byte range only means anything against the raw object, so a request
	// that asks for one gives up the compressed representation.
	allowGzip := request.Header.Get("Range") == "" && acceptsGzip(request.Header.Get("Accept-Encoding"))
	content, file, err := s.store.OpenContent(request.Context(), request.PathValue("id"), allowGzip)
	// Deferred before the name check, which rejects a request that already
	// opened the object and would otherwise leak the handle.
	if err == nil {
		defer content.Close()
	}
	if errors.Is(err, store.ErrNotFound) || file.Name != request.PathValue("name") {
		http.NotFound(writer, request)
		return
	}
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "Could not open this file.")
		return
	}

	etag := `"` + file.SHA256 + `"`
	writer.Header().Set("Content-Type", file.ContentType)
	writer.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": file.Name}))
	writer.Header().Set("Cache-Control", "private, no-cache")
	// Both representations decode to the same bytes and so share one entity
	// tag; Vary is what keeps a cache from handing the compressed copy to a
	// client that cannot decode it. The offline service worker relies on the
	// tag matching the file's SHA-256 to revalidate its cached copy.
	writer.Header().Set("ETag", etag)
	writer.Header().Add("Vary", "Accept-Encoding")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	if content.Encoding == "" {
		http.ServeContent(writer, request, file.Name, file.UpdatedAt, content)
		return
	}
	// The identity branch gets its validator from http.ServeContent; the
	// compressed branch sets one so both representations answer conditional
	// requests the same way.
	writer.Header().Set("Last-Modified", file.UpdatedAt.UTC().Format(http.TimeFormat))
	if status := conditionalStatus(request, etag, file.UpdatedAt); status != 0 {
		writer.WriteHeader(status)
		return
	}
	writer.Header().Set("Content-Encoding", content.Encoding)
	writer.Header().Set("Content-Length", strconv.FormatInt(content.Size, 10))
	// A GET route also matches HEAD, and the server discards the body for it.
	// Reading the sidecar anyway would cost a full-file disk read per request.
	if request.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(writer, content)
}

// conditionalStatus evaluates the request's preconditions in the order RFC
// 9110 requires and reports the status to send instead of the body, or zero to
// serve it. http.ServeContent does this for the identity representation; the
// compressed one must not answer differently merely because the client can
// decode gzip.
//
// Entity tags are compared weakly throughout. If-Match calls for strong
// comparison, but this resource is read-only, so the leniency can only ever
// serve a representation where a strict server would refuse.
func conditionalStatus(request *http.Request, etag string, modtime time.Time) int {
	modtime = modtime.Truncate(time.Second)
	if match := request.Header.Get("If-Match"); match != "" {
		if !etagListMatches(match, etag) {
			return http.StatusPreconditionFailed
		}
	} else if since, err := http.ParseTime(request.Header.Get("If-Unmodified-Since")); err == nil {
		if modtime.After(since) {
			return http.StatusPreconditionFailed
		}
	}
	if none := request.Header.Get("If-None-Match"); none != "" {
		if etagListMatches(none, etag) {
			return http.StatusNotModified
		}
		return 0
	}
	if since, err := http.ParseTime(request.Header.Get("If-Modified-Since")); err == nil && !modtime.After(since) {
		return http.StatusNotModified
	}
	return 0
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	if writer.Header().Get("Content-Type") == "" {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		return
	}
}

func writeProblem(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}
