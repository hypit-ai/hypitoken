// Package gptpay serves an isolated checkout form and its optional Go backend.
package gptpay

import (
	"bytes"
	"embed"
	"net/http"
	"strings"
	"time"
)

//go:embed web/* web/fonts/*
var assets embed.FS

// NewHandler returns an isolated handler safe to mount on its own
// listener. Explicit paths prevent directory listings and SPA fallback into
// unrelated APIs. No external resources or analytics are loaded by the page.
func NewHandler(services ...*Service) http.Handler {
	var service *Service
	if len(services) > 0 {
		service = services[0]
	}
	files := map[string]struct{ name, mime string }{
		"/":                       {"index.html", "text/html; charset=utf-8"},
		"/assets/style.css":       {"style.css", "text/css; charset=utf-8"},
		"/assets/tokens.css":      {"tokens.css", "text/css; charset=utf-8"},
		"/assets/app.mjs":         {"app.mjs", "text/javascript; charset=utf-8"},
		"/assets/devfill.mjs":     {"devfill.mjs", "text/javascript; charset=utf-8"},
		"/assets/validation.mjs":  {"validation.mjs", "text/javascript; charset=utf-8"},
		"/assets/bricolage.woff2": {"fonts/bricolage.woff2", "font/woff2"},
		"/assets/jetbrains.woff2": {"fonts/jetbrains.woff2", "font/woff2"},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			service.serve(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				writeJSON(w, 200, map[string]any{"status": "ok", "endpoint": "gptpay", "enabled": service != nil})
			}
			return
		}
		file, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		data, err := assets.ReadFile("web/" + file.name)
		if err != nil {
			http.Error(w, "asset unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", file.mime)
		http.ServeContent(w, r, file.name, time.Time{}, bytes.NewReader(data))
	})
}
