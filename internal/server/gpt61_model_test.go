package server

import (
	"math"
	"testing"
	"time"

	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/pricing"
	"github.com/wjsoj/cc-core/usage"
)

// An API launch does not prove OAuth plan eligibility. A verified API-key
// catalog must route this new SKU without substituting an older Sol model.
func TestGPT61SolVerifiedAPIKeyRoutingAndBilling(t *testing.T) {
	relay := &auth.Auth{ID: "sol61", Kind: auth.KindAPIKey, Provider: auth.ProviderOpenAI, AllowedModels: []string{"gpt-6.1-sol"}}
	s := &Server{pool: auth.NewPool(nil, []*auth.Auth{relay}, time.Minute, false, "")}
	s.codexModels.models = map[string]bool{"gpt-6.1-sol": true}
	s.codexModels.fetchedAt = time.Now()
	cat := pricing.NewCatalog(pricing.Config{})
	for _, model := range []string{"gpt-6.1-sol", "gpt-6.1-sol(max)", "gpt-6.1-sol-high"} {
		route := s.guardCodexModel(model)
		if route.reject != nil || !route.apiKeyOnly {
			t.Fatalf("model %s did not select the verified API-key route: %+v", model, route)
		}
		billed := billingModelFor(relay, model)
		got := cat.Cost("openai", billed, usage.Counts{InputTokens: 1000000, OutputTokens: 1000000, CacheReadTokens: 1000000, CacheCreateTokens: 1000000})
		if math.Abs(got-14.6) > 1e-9 {
			t.Fatalf("%s cost = %g, want 14.6 (cache read must be $0.10/MTok)", model, got)
		}
	}
}
