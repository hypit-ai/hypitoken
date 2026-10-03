package server

import (
	"context"
	"testing"
	"time"

	"github.com/wjsoj/cc-core/auth"
)

func TestCodexAcquisitionHonorsEachOAuthPlan(t *testing.T) {
	for _, model := range []string{"gpt-5.6-sol", "gpt-5.6-sol(1m)", "gpt-5.6-sol-high"} {
		t.Run(model, func(t *testing.T) {
			free, paid := wsCred("free"), wsCred("paid")
			free.PlanType, paid.PlanType = "free", "pro"
			s := codexGuardServer(free, paid)
			// The fleet catalog advertises the paid model, while the client
			// already has a sticky session on a Free account for another model.
			if s.guardCodexModel(model).reject != nil {
				t.Fatal("paid model should be available in the fleet")
			}
			ctx := context.Background()
			first := s.pool.Acquire(ctx, auth.ProviderOpenAI, "client", "", "gpt-6-luna", "session", paid.ID)
			if first != free {
				t.Fatal("failed to establish the Free-account sticky session")
			}
			got := s.acquireWithAPIKeyRecovery(ctx, auth.ProviderOpenAI, "client", "", model, "session", auth.AcquireOptions{AllowAPIKeyFallback: true})
			if got != paid {
				t.Fatalf("paid model routed to %v instead of its eligible account", got)
			}
		})
	}
}

func TestCodexPlanFilterPreservesFallbackAndGroupBoundaries(t *testing.T) {
	free := wsCred("free")
	free.PlanType = "free"
	other := wsCred("paid-other-group")
	other.PlanType, other.Group = "pro", "private"
	key := &auth.Auth{ID: "relay", Kind: auth.KindAPIKey, Provider: auth.ProviderOpenAI, AllowedModels: []string{"gpt-5.6-sol"}}
	s := codexGuardServer(free, other)
	s.pool = auth.NewPool([]*auth.Auth{free, other}, []*auth.Auth{key}, time.Minute, false, "")
	opts := auth.AcquireOptions{AllowAPIKeyFallback: true}
	got := s.acquireWithAPIKeyRecovery(context.Background(), auth.ProviderOpenAI, "client", "", "gpt-5.6-sol", "session", opts)
	if got != key {
		t.Fatal("a paid model must use the eligible API key, without crossing groups")
	}
	opts.ExcludeIDs = []string{key.ID}
	if got := s.acquireWithAPIKeyRecovery(context.Background(), auth.ProviderOpenAI, "client2", "", "gpt-5.6-sol", "session2", opts); got != nil {
		t.Fatal("exhaustion must not fall back to a Free account or another group")
	}
}

func TestCodexPlanFilterKeepsFreeModelsAndUnknownPlans(t *testing.T) {
	for _, tc := range []struct{ plan, model string }{
		{"free", "gpt-6-luna"}, {"free", "gpt-6-astra"},
		{"", "gpt-5.6-sol"}, {"future-plan", "gpt-5.6-sol"},
		{"prolite", "gpt-5.6-sol"}, {"free", "codex-auto-review"},
	} {
		t.Run(tc.plan+"/"+tc.model, func(t *testing.T) {
			a := wsCred("account")
			a.PlanType = tc.plan
			s := codexGuardServer(a)
			got := s.acquireWithAPIKeyRecovery(context.Background(), auth.ProviderOpenAI, "client", "", tc.model, "session", auth.AcquireOptions{})
			if got != a {
				t.Fatal("compatible model or unknown-plan fallback was blocked")
			}
		})
	}
}
