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

func TestCodexExplicitFailuresOnlyKeepsModelFailureRoutable(t *testing.T) {
	s := &Server{}
	a := &auth.Auth{ID: "model-relay", Kind: auth.KindAPIKey, Provider: auth.ProviderOpenAI, ExplicitFailuresOnly: true}
	for i := 0; i < 20; i++ {
		s.reportCodexAPIKeyFault(a, 503, time.Time{}, []byte(`{"error":{"code":"model_not_found","message":"No available channel for model gpt-5.6-terra"}}`))
		s.reportCodexAPIKeyFault(a, 502, time.Time{})
	}
	if !a.IsHealthy() {
		t.Fatal("ambiguous model/transport errors paused the whole relay")
	}
	if until, strikes := a.QuarantineSnapshot(); !until.IsZero() || strikes != 0 {
		t.Fatal("ambiguous failures accumulated breaker strikes")
	}
	s.reportCodexAPIKeyFault(a, 403, time.Time{}, []byte(`{"error":{"message":"insufficient account balance"}}`))
	if !a.IsQuarantined(time.Now()) {
		t.Fatal("explicit balance failure did not pause")
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
		{"ambiguous forbidden", 403, `{"error":{"message":"upstream unavailable"}}`, false},
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
			if paused := a.IsQuarantined(time.Now()); paused != tc.pause {
				t.Fatalf("paused=%v want %v", paused, tc.pause)
			}
		})
	}
}
