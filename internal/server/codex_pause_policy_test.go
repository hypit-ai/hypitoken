package server

import (
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wjsoj/cc-core/auth"
)

func TestCodexExplicitFailuresOnlyCannotBypassBackoff(t *testing.T) {
	s := &Server{}
	a := &auth.Auth{ID: "relay", Kind: auth.KindAPIKey, Provider: auth.ProviderOpenAI, ExplicitFailuresOnly: true}
	for i := 0; i < 3; i++ {
		s.reportCodexAPIKeyFault(a, 503, time.Time{})
	}
	if !a.IsQuarantined(time.Now()) || a.Snapshot().Disabled {
		t.Fatal("legacy pause opt-out must not keep a repeatedly failing key routable")
	}
}

func TestCodexPausePolicyClassifiesCompressedUpstreamErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		pause  bool
	}{
		{"model unavailable", 503, `{"error":{"code":"model_not_found","message":"No available channel for model"}}`, false},
		{"ambiguous forbidden", 403, `{"error":{"message":"upstream unavailable"}}`, true},
		{"explicit balance", 403, `{"error":{"message":"insufficient account balance"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				gz := gzip.NewWriter(w)
				if _, err := gz.Write([]byte(tc.body)); err != nil {
					t.Error(err)
				}
				if err := gz.Close(); err != nil {
					t.Error(err)
				}
			}))
			defer upstream.Close()
			a := &auth.Auth{ID: "relay", Kind: auth.KindAPIKey, Provider: auth.ProviderOpenAI, BaseURL: upstream.URL, AccessToken: "test", ExplicitFailuresOnly: true}
			s := codexHTTPTestServer(upstream.URL, a)
			for i := 0; i < 4; i++ {
				body := `{"model":"gpt-5.6-sol","input":"hello"}`
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
				retry, done := s.doForwardCodex(c, a, "/v1/responses", []byte(body), false, "gpt-5.6-sol", "test-client", "test-client", "slot", time.Now(), i+1)
				if !retry || done {
					t.Fatalf("failed request must remain retryable: retry=%v done=%v", retry, done)
				}
			}
			if a.IsQuarantined(time.Now()) != tc.pause || a.Snapshot().Disabled {
				t.Fatalf("paused=%v want %v; disabled=%v", a.IsQuarantined(time.Now()), tc.pause, a.Snapshot().Disabled)
			}
		})
	}
}
