package adapter

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/wjsoj/CPA-Claude/internal/saas/db"
	"github.com/wjsoj/CPA-Claude/internal/server"
	"github.com/wjsoj/cc-core/pricing"
	"github.com/wjsoj/cc-core/usage"
)

func TestGPT6ChargesWorkspaceMultiplierOnce(t *testing.T) {
	for _, model := range []string{"gpt-6-sol", "gpt-6-luna"} {
		for _, multiplier := range []float64{0, .04} {
			t.Run(fmt.Sprintf("%s/%g", model, multiplier), func(t *testing.T) {
				f := newChargeFixture(t, 100)
				ctx := context.Background()
				if err := f.ad.DB.UpdateWorkspace(ctx, f.wsID, db.WorkspaceUpdate{CodexMultiplier: &multiplier}); err != nil {
					t.Fatal(err)
				}
				info, ok := f.ad.LookupByTokenID(ctx, f.info.TokenID)
				if !ok {
					t.Fatal("token did not resolve")
				}
				counts := usage.Counts{InputTokens: 200000, CacheReadTokens: 60000, CacheCreateTokens: 12001, OutputTokens: 1000, Requests: 1}
				cat := pricing.NewCatalog(pricing.Config{})
				for _, oauth := range []bool{false, true} {
					base := cat.CostWithOptions("openai", model, counts, pricing.CostOptions{ServiceTier: "priority", CodexOAuth: oauth}).CostUSD
					rate := multiplier
					if rate == 0 {
						rate = .05
					}
					want := pricing.QuantizeUSD(base * rate)
					before := f.balance(t)
					chargeCtx := server.WithChargeIdemKey(ctx, fmt.Sprintf("gpt6:%s:%t", model, oauth))
					got, err := f.ad.Charge(chargeCtx, info, "openai", model, counts, base)
					if err != nil {
						t.Fatal(err)
					}
					if math.Abs(got-want) > 1e-9 || math.Abs(before-f.balance(t)-want) > 1e-9 {
						t.Fatalf("debit=%g expected=%g balance delta=%g", got, want, before-f.balance(t))
					}
					after := f.balance(t)
					if _, err := f.ad.Charge(chargeCtx, info, "openai", model, counts, base); err != nil {
						t.Fatal(err)
					}
					if f.balance(t) != after {
						t.Fatal("retry charged twice")
					}
				}
			})
		}
	}
}
