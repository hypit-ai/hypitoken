package admin

import (
	"testing"
	"time"

	"github.com/wjsoj/cc-core/auth"
)

// The derivations themselves (PurchasedAt/IsFree/AtRisk) are cc-core's and
// tested there. What is pinned here is the adapter: a never-probed credential
// must produce no view at all — an empty struct would render as a billing panel
// full of dashes and an "as of 0001-01-01" timestamp — and a real payload must
// carry the at-risk deadline through, since that value drives an operator-
// facing alert on the credential card.

func TestCodexSubscriptionViewNilForUnprobed(t *testing.T) {
	if v := newCodexSubscriptionView(nil, time.Time{}); v != nil {
		t.Fatalf("unprobed credential produced a view: %+v", v)
	}
}

func TestCodexSubscriptionViewCarriesDerivedFields(t *testing.T) {
	start := time.Date(2026, 8, 4, 10, 22, 17, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	fetched := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	info := &auth.CodexSubscriptionInfo{
		Portal: &auth.CodexSubscriptionPortal{
			PlanType:    "plus",
			ActiveStart: start,
			ActiveUntil: end,
			WillRenew:   false, // cancelled at term end
		},
		Entitlement: &auth.CodexEntitlement{
			HasActiveSubscription: true,
			// Free via a 100%-off promo rather than the gratis flag — the case
			// a naive "is it free" check misses.
			Discount: &auth.CodexDiscount{
				DiscountType:    "percentage",
				Amount:          100,
				PromoCampaignID: "plus-1-month-free",
			},
		},
	}

	v := newCodexSubscriptionView(info, fetched)
	if v == nil {
		// t.Fatal exits via runtime.Goexit, not panic/os.Exit, so SA5011
		// (staticcheck) doesn't credit it as terminating this function — an
		// explicit return makes every v.* access below provably guarded
		// without relying on that recognition.
		t.Fatal("probed credential produced no view")
		return
	}
	if v.Plan != "plus" {
		t.Errorf("plan = %q, want plus", v.Plan)
	}
	if v.PurchasedAt == nil || !v.PurchasedAt.Equal(start) {
		t.Errorf("purchased_at = %v, want %v", v.PurchasedAt, start)
	}
	if v.ExpiresAt == nil || !v.ExpiresAt.Equal(end) {
		t.Errorf("expires_at = %v, want %v", v.ExpiresAt, end)
	}
	if !v.Free || v.FreeReason != "promo:plus-1-month-free" {
		t.Errorf("free = %v/%q, want true/promo:plus-1-month-free", v.Free, v.FreeReason)
	}
	if !v.AtRisk || v.RiskReason != "will_not_renew" {
		t.Errorf("at_risk = %v/%q, want true/will_not_renew", v.AtRisk, v.RiskReason)
	}
	if v.RiskDeadline == nil || !v.RiskDeadline.Equal(end) {
		t.Errorf("risk_deadline = %v, want %v", v.RiskDeadline, end)
	}
	if v.FetchedAt == nil || !v.FetchedAt.Equal(fetched) {
		t.Errorf("fetched_at = %v, want %v", v.FetchedAt, fetched)
	}
}

// A never-paid free account satisfies will_renew == false trivially and has no
// term to lose. Reporting it as about to lapse would put a permanent red alert
// on every free credential in the fleet.
func TestCodexSubscriptionViewFreeAccountNotAtRisk(t *testing.T) {
	v := newCodexSubscriptionView(&auth.CodexSubscriptionInfo{
		Account:     &auth.CodexBillingAccount{PlanType: "free"},
		Entitlement: &auth.CodexEntitlement{HasActiveSubscription: false},
		LastActive:  &auth.CodexLastActiveSubscription{WillRenew: false},
	}, time.Now())
	if v == nil {
		t.Fatal("probed credential produced no view")
		return // see the matching comment in TestCodexSubscriptionViewCarriesDerivedFields
	}
	if v.AtRisk {
		t.Errorf("never-paid free account flagged at risk (%q, %v)", v.RiskReason, v.RiskDeadline)
	}
	if v.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil for an account with no term", v.ExpiresAt)
	}
}
