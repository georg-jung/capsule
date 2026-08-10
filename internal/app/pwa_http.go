package app

import "net/http"

func (s *Server) handleServiceWorker(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Service-Worker-Allowed", "/")
	writeStaticAsset(writer, request, s.swAsset)
}

func (s *Server) handleManifest(writer http.ResponseWriter, request *http.Request) {
	instance, err := s.store.Instance(request.Context())
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "Manifest unavailable.")
		return
	}
	writer.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
	writer.Header().Set("Cache-Control", "private, no-cache")
	writeJSON(writer, http.StatusOK, map[string]any{
		"id":               "/app",
		"name":             instance.SiteName,
		"short_name":       instance.SiteName,
		"description":      "Private offline file capsule",
		"start_url":        "/app",
		"scope":            "/",
		"display":          "standalone",
		"background_color": "#f4f3ef",
		"theme_color":      "#20201e",
		"icons": []map[string]string{
			{"src": "/assets/icon-192.png", "sizes": "192x192", "type": "image/png", "purpose": "any"},
			{"src": "/assets/icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "any maskable"},
			{"src": "/assets/icon.svg", "sizes": "any", "type": "image/svg+xml", "purpose": "any maskable"},
		},
	})
}
