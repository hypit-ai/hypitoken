package server

import (
	"testing"

	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/pricing"
	"github.com/wjsoj/cc-core/usage"
)

// Billing prices on the model we actually bought upstream; display keeps the
// client-facing name. The split only applies to OAuth credentials — an API-key
// relay's model_map is a vendor naming convention whose targets match no price
// card, so pricing on those would drop every request to the provider default.
func TestBillingModelFor(t *testing.T) {
	oauth := &auth.Auth{
		ID:       "oauth",
		Kind:     auth.KindOAuth,
		Provider: auth.ProviderAnthropic,
		ModelMap: map[string]string{
			"claude-opus-4-7":   "claude-opus-5",
			"claude-sonnet-4-6": "claude-sonnet-5",
		},
	}
	relay := &auth.Auth{
		ID:       "relay",
		Kind:     auth.KindAPIKey,
		Provider: auth.ProviderAnthropic,
		ModelMap: map[string]string{
			"claude-sonnet-4-6": "[0.1]a/claude-sonnet-4-6",
		},
	}
	plain := &auth.Auth{ID: "plain", Kind: auth.KindOAuth, Provider: auth.ProviderAnthropic}

	for _, tc := range []struct {
		name       string
		a          *auth.Auth
		client     string
		wantBilled string
	}{
		// OAuth: bill on the folded-to model.
		{"oauth opus fold", oauth, "claude-opus-4-7", "claude-opus-5"},
		{"oauth sonnet fold", oauth, "claude-sonnet-4-6", "claude-sonnet-5"},
		// Dated + [1m] variants resolve through the same entries (cc-core's
		// prefix fallback), so they must not fall back to the client name.
		{"oauth dated variant", oauth, "claude-opus-4-7-20260315", "claude-opus-5"},
		{"oauth 1m variant", oauth, "claude-sonnet-4-6[1m]", "claude-sonnet-5[1m]"},
		// Unmapped model on an OAuth credential passes through.
		{"oauth unmapped", oauth, "claude-haiku-4-5", "claude-haiku-4-5"},
		{"oauth no map at all", plain, "claude-opus-4-7", "claude-opus-4-7"},
		// API-key relay: NEVER bill on the vendor-prefixed upstream name.
		{"relay keeps client name", relay, "claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"nil credential", nil, "claude-opus-5", "claude-opus-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := billingModelFor(tc.a, tc.client); got != tc.wantBilled {
				t.Errorf("billingModelFor(%s, %q) = %q, want %q",
					tc.name, tc.client, got, tc.wantBilled)
			}
		})
	}
}

// A new Opus must retain its own lower price on OAuth and vendor-prefixed
// API-key relays, including the long-context label.
func TestOpus55BillingModel(t *testing.T) {
	cat := pricing.NewCatalog(pricing.Config{})
	for _, kind := range []auth.Kind{auth.KindOAuth, auth.KindAPIKey} {
		a := &auth.Auth{Kind: kind, Provider: auth.ProviderAnthropic}
		if kind == auth.KindAPIKey {
			a.ModelMap = map[string]string{"claude-opus-5-5": "vendor/claude-opus-5-5"}
		}
		for _, model := range []string{"claude-opus-5-5", "claude-opus-5-5[1m]"} {
			billed := billingModelFor(a, model)
			counts := usage.Counts{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000, CacheCreateTokens: 2_000_000, CacheCreate1hTokens: 1_000_000}
			if got := cat.Cost(auth.ProviderAnthropic, billed, counts); got != 37.2 {
				t.Fatalf("kind=%d model=%s billed=%s cost=%g, want 37.2", kind, model, billed, got)
			}
		}
	}
}

func TestOpus55ObservedUsageBilling(t *testing.T) {
	const rawUsage = `{"input_tokens":300000,"output_tokens":10000,"cache_read_input_tokens":400000,"cache_creation_input_tokens":200000,"cache_creation":{"ephemeral_5m_input_tokens":50000,"ephemeral_1h_input_tokens":150000}}`
	nonstream := extractUsageFromJSON([]byte(`{"usage":`+rawUsage+`}`), nil)
	var stream usage.Counts
	mergeSSEUsage(&stream, nil, []byte(`{"type":"message_start","message":{"usage":`+rawUsage+`}}`))
	cat := pricing.NewCatalog(pricing.Config{})
	for _, counts := range []usage.Counts{nonstream, stream} {
		if got := cat.Cost(auth.ProviderAnthropic, "claude-opus-5-5", counts); got != 2.93 {
			t.Fatalf("observed counts=%+v cost=%g, want 2.93", counts, got)
		}
	}
}
