package adapter

import (
	"context"
	"math"
	"testing"

	"github.com/wjsoj/CPA-Claude/internal/saas/db"
	"github.com/wjsoj/CPA-Claude/internal/server"
	"github.com/wjsoj/cc-core/pricing"
	"github.com/wjsoj/cc-core/usage"
)

func TestOpus55WorkspaceCharge(t *testing.T) {
	f := newChargeFixture(t, 100)
	ctx := context.Background()
	multiplier := .4
	if err := f.ad.DB.UpdateWorkspace(ctx, f.wsID, db.WorkspaceUpdate{ClaudeMultiplier: &multiplier}); err != nil {
		t.Fatal(err)
	}
	info, ok := f.ad.LookupByTokenID(ctx, f.info.TokenID)
	if !ok {
		t.Fatal("token did not resolve")
	}
	counts := usage.Counts{InputTokens: 300000, OutputTokens: 10000, CacheReadTokens: 400000, CacheCreateTokens: 200000, CacheCreate1hTokens: 150000, Requests: 1}
	base := pricing.NewCatalog(pricing.Config{}).Cost("anthropic", "claude-opus-5-5[1m]", counts)
	if base != 2.93 {
		t.Fatalf("base=%g, want 2.93", base)
	}
	ctx = server.WithChargeIdemKey(ctx, "opus55:main")
	before := f.balance(t)
	got, err := f.ad.Charge(ctx, info, "anthropic", "claude-opus-5-5", counts, base)
	if err != nil {
		t.Fatal(err)
	}
	const want = 1.172
	if math.Abs(got-want) > 1e-9 || math.Abs(before-f.balance(t)-want) > 1e-9 {
		t.Fatalf("charged=%g balance delta=%g, want %g", got, before-f.balance(t), want)
	}
	after := f.balance(t)
	if _, err := f.ad.Charge(ctx, info, "anthropic", "claude-opus-5-5", counts, base); err != nil {
		t.Fatal(err)
	}
	if f.balance(t) != after {
		t.Fatal("retry charged twice")
	}
}
