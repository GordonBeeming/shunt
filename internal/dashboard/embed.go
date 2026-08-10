package dashboard

import (
	"embed"
	"net/http"
)

//go:embed assets/index.html
var assets embed.FS

// handleIndex serves the single-page UI.
func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := assets.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	// Don't let the browser cache the page — a stale copy loaded over http would
	// keep polling http against the https-only server and read as "unreachable".
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}
