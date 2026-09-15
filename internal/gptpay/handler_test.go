package gptpay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAssetsAndSecurity(t *testing.T) {
	h := NewHandler()
	for _, path := range []string{"/", "/healthz", "/assets/style.css", "/assets/tokens.css", "/assets/app.mjs", "/assets/devfill.mjs", "/assets/validation.mjs", "/assets/bricolage.woff2", "/assets/jetbrains.woff2"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != 200 || w.Body.Len() == 0 {
				t.Fatalf("GET %s: %d", path, w.Code)
			}
			if !strings.Contains(w.Header().Get("Content-Security-Policy"), "connect-src 'self'") {
				t.Fatal("only same-origin API requests are allowed")
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("page must not be cached")
			}
			if w.Header().Get("X-Frame-Options") != "DENY" {
				t.Fatal("must not be framed")
			}
		})
	}
}

func TestNoAPIsOrSensitiveSubmission(t *testing.T) {
	h := NewHandler()
	for _, path := range []string{"/admin/api/summary", "/v1/checkout/sessions", "/assets/", "/config.yaml", "/assets/../handler.go"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != 404 {
			t.Fatalf("unexpected route %s: %d", path, w.Code)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/", strings.NewReader("secret-input")))
		if w.Code != 405 || strings.Contains(w.Body.String(), "secret-input") {
			t.Fatal("submission accepted or echoed")
		}
	}
}

func TestHeadHasNoBody(t *testing.T) {
	for _, path := range []string{"/", "/healthz", "/assets/app.mjs"} {
		w := httptest.NewRecorder()
		NewHandler().ServeHTTP(w, httptest.NewRequest(http.MethodHead, path, nil))
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatalf("HEAD %s: %d, body %d", path, w.Code, w.Body.Len())
		}
	}
}
