package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wjsoj/CPA-Claude/internal/saas/db"
	"github.com/wjsoj/cc-core/auth"
)

func TestProbeRequiresCompletedAccountedResponse(t *testing.T) {
	for _, tc := range []struct {
		name, provider, body string
		valid                bool
	}{
		{"opener is not success", "openai", "data: {\"type\":\"response.created\"}\n\n", false},
		{"missing usage", "openai", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", false},
		{"null usage counts", "openai", `{"status":"completed","usage":{"input_tokens":null,"output_tokens":null}}`, false},
		{"failed SSE", "openai", "data: {\"type\":\"response.failed\"}\n\n", false},
		{"complete SSE", "openai", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":9,\"output_tokens\":2}}}\n\n", true},
		{"complete JSON", "openai", `{"status":"completed","usage":{"input_tokens":9,"output_tokens":2}}`, true},
		{"HTML success", "openai", "<html>unavailable</html>", false},
		{"Claude complete", "anthropic", "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n", true},
		{"Claude truncated", "anthropic", "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":1}}}\n\n", false},
		{"Claude JSON", "anthropic", `{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":2}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProbeResponse(strings.NewReader(tc.body), tc.provider)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestProbeUsesAllowedModelsAndProductionURL(t *testing.T) {
	for _, suffix := range []string{"", "/v1", "/custom"} {
		t.Run(suffix, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := "/v1/responses"
				if suffix == "/custom" {
					want = "/custom/responses"
				}
				if r.URL.Path != want {
					t.Errorf("path=%s want=%s", r.URL.Path, want)
				}
				var b map[string]any
				if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
					t.Error(err)
				}
				if b["model"] != "vendor-sol" {
					t.Errorf("model=%v", b["model"])
				}
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"))
			}))
			defer upstream.Close()
			a := &auth.Auth{Kind: auth.KindAPIKey, Provider: auth.ProviderOpenAI, BaseURL: upstream.URL + suffix, AccessToken: "test", AllowedModels: []string{"gpt-6-sol"}, ModelMap: map[string]string{"gpt-6-sol": "vendor-sol"}}
			if got := probeModels(a, auth.ProviderOpenAI); !reflect.DeepEqual(got, []string{"gpt-6-sol"}) {
				t.Fatal(got)
			}
			status, reason := (&Checker{}).probe(t.Context(), a, auth.ProviderOpenAI, "gpt-6-sol")
			if status != "ok" {
				t.Fatalf("%s: %s", status, reason)
			}
		})
	}
}

func TestDisabledAndPassiveChannelsDoNotProbe(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer upstream.Close()
	disabled := &auth.Auth{ID: "disabled", Kind: auth.KindAPIKey, Provider: auth.ProviderOpenAI, BaseURL: upstream.URL, Disabled: true}
	passive := &auth.Auth{ID: "cli", Kind: auth.KindAPIKey, Provider: auth.ProviderAnthropic, BaseURL: upstream.URL, AllowedModels: []string{"claude-opus-5"}}
	store, err := db.Open(filepath.Join(t.TempDir(), "saas.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pool := auth.NewPool(nil, []*auth.Auth{disabled, passive}, time.Minute, false, "")
	c := New(store, pool, time.Minute, "")
	c.PassiveAuthIDs = []string{"cli"}
	c.RunOnce(context.Background())
	if calls.Load() != 0 {
		t.Fatalf("unexpected probes: %d", calls.Load())
	}
	rows, err := store.ListModelHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AuthID != "cli" || rows[0].Status != "unknown" {
		t.Fatalf("health=%+v", rows)
	}
}
