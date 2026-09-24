package adapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wjsoj/cc-core/pricing"
)

// publicPricingHandler mirrors the closure Mount registers at
// GET /api/v2/pricing. Mount needs a fully-wired dependency graph (DB, issuer,
// six sub-handlers) that these tests have no use for, so the body is duplicated
// here and TestPublicPricingRouteIsUnauthenticated checks the real route still
// exists on the public group — which is the part a drifting copy would break.
func publicPricingHandler(catalog *pricing.Catalog) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"default":           catalog.Default(),
			"provider_defaults": catalog.ProviderDefaults(),
			"models":            catalog.Models(),
		})
	}
}

type publicCatalogue struct {
	Default          pricing.ModelPrice            `json:"default"`
	ProviderDefaults map[string]pricing.ModelPrice `json:"provider_defaults"`
	Models           map[string]pricing.ModelPrice `json:"models"`
}

func fetchPublicPricing(t *testing.T, cat *pricing.Catalog) publicCatalogue {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v2/pricing", publicPricingHandler(cat))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v2/pricing", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var out publicCatalogue
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// TestPublicPricingServesEveryBillableModel is the regression barrier for the
// bug this endpoint exists to close: the /pricing page carried its own
// hardcoded copy of the rate table, and it drifted out of step with what the
// billing path actually charged. claude-opus-5 and claude-fable-5 were billed
// at $5/$25 and $10/$50 while appearing on no published price list, and the
// gpt-5.6 tiers advertised no cache-write rate despite being charged one.
//
// Serving the catalogue verbatim makes "priced but unlisted" impossible by
// construction. This test pins that: every model the catalogue can bill must
// come back over the wire, at the identical rate.
func TestPublicPricingServesEveryBillableModel(t *testing.T) {
	cat := pricing.NewCatalog(pricing.Config{})
	got := fetchPublicPricing(t, cat)

	want := cat.Models()
	if len(got.Models) != len(want) {
		t.Fatalf("served %d models, catalogue has %d", len(got.Models), len(want))
	}
	for key, card := range want {
		served, ok := got.Models[key]
		if !ok {
			t.Errorf("%s is billable but absent from the public price list", key)
			continue
		}
		if served != card {
			t.Errorf("%s served %+v, billed %+v", key, served, card)
		}
	}

	// The two models whose absence prompted this change, named explicitly so a
	// future catalogue edit that drops them fails loudly here.
	for _, m := range []string{"anthropic/claude-opus-5", "anthropic/claude-fable-5"} {
		if _, ok := got.Models[m]; !ok {
			t.Errorf("%s missing — this is the exact gap the endpoint was added to close", m)
		}
	}

	// Fallback cards must ship too: a model resolved through the provider
	// default still bills at a real rate, and the page has to be able to say so.
	if len(got.ProviderDefaults) == 0 {
		t.Error("provider_defaults empty — models resolved by fallback would show no price")
	}
	if got.Default.InputPer1M == 0 {
		t.Error("default card missing an input rate")
	}
}

// The cache axes are the majority of a Claude Code bill, so they have to
// survive serialization intact — a page that renders "—" for a rate that is
// actually charged is the same class of bug as omitting the model.
func TestPublicPricingCarriesCacheRates(t *testing.T) {
	got := fetchPublicPricing(t, pricing.NewCatalog(pricing.Config{}))

	sonnet, ok := got.Models["anthropic/claude-sonnet-4-6"]
	if !ok {
		t.Fatal("anthropic/claude-sonnet-4-6 missing")
	}
	if sonnet.CacheReadPer1M == 0 || sonnet.CacheCreatePer1M == 0 {
		t.Errorf("sonnet-4-6 cache rates lost in transit: %+v", sonnet)
	}

	// gpt-5.6 is the line that exposed the gap: the catalogue charges a
	// cache-write rate the hardcoded page rendered as "—". gpt-6-astra joined
	// them on 2026-09-05 — it publishes a cache-write rate too ($12.50/1M,
	// 1.25x input), and it is the flagship, so a "—" there is the costliest
	// version of the same bug.
	for _, m := range []string{"openai/gpt-6-sol", "openai/gpt-6-luna", "openai/gpt-6-astra", "openai/gpt-5.6-sol", "openai/gpt-5.6-terra", "openai/gpt-5.6-luna"} {
		card, ok := got.Models[m]
		if !ok {
			t.Errorf("%s missing", m)
			continue
		}
		if card.CacheCreatePer1M == 0 {
			t.Errorf("%s served no cache-write rate but the catalogue charges %v",
				m, card.CacheCreatePer1M)
		}
	}
}

// A config override must reach the page, not just the biller — otherwise an
// operator repricing a model in config.yaml would silently advertise the old
// number while charging the new one.
func TestPublicPricingReflectsConfigOverrides(t *testing.T) {
	cat := pricing.NewCatalog(pricing.Config{Models: map[string]pricing.ModelPrice{
		"anthropic/claude-sonnet-4-6": {InputPer1M: 9.99, OutputPer1M: 42, CacheReadPer1M: 1, CacheCreatePer1M: 2},
	}})
	got := fetchPublicPricing(t, cat)
	if got.Models["anthropic/claude-sonnet-4-6"].InputPer1M != 9.99 {
		t.Fatalf("override not served: %+v", got.Models["anthropic/claude-sonnet-4-6"])
	}
}

// The route must stay on the unauthenticated group: /pricing is linked from the
// logged-out landing page, and a 401 there renders an empty table rather than a
// wrong one — recoverable, but still a broken price list.
func TestPublicPricingRouteIsUnauthenticated(t *testing.T) {
	src, err := readRouterSource()
	if err != nil {
		t.Skipf("router source unavailable: %v", err)
	}
	idx := strings.Index(src, `v2.GET("/pricing"`)
	if idx < 0 {
		t.Fatal(`no v2.GET("/pricing") in router.go — the public catalogue route is gone`)
	}
	// `authed` is the JWT-guarded sub-group; the public routes hang off v2.
	if strings.Contains(src[idx:idx+40], "authed") {
		t.Error("/pricing moved onto the authenticated group")
	}
}

func readRouterSource() (string, error) {
	b, err := os.ReadFile("router.go")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func TestPublicPricingOpus55(t *testing.T) {
	got := fetchPublicPricing(t, pricing.NewCatalog(pricing.Config{}))
	want := pricing.ModelPrice{InputPer1M: 4, OutputPer1M: 20, CacheReadPer1M: .2, CacheCreatePer1M: 5, CacheCreate1hPer1M: 8}
	if card, ok := got.Models["anthropic/claude-opus-5-5"]; !ok || card != want {
		t.Fatalf("Opus 5.5 public price=%+v, present=%v, want %+v", card, ok, want)
	}
}
