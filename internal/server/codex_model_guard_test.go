package server

import (
	"testing"
	"time"

	"github.com/wjsoj/cc-core/auth"
)

// The corpus is production's own tail: every model name that reached the Codex
// endpoint and produced zero output in the entire archive, paired with what the
// customer plainly meant. Before the guard each of these cost a four-minute
// wait across the whole credential pool and an error that named nothing.
func TestClosestModelNameOnProductionTypos(t *testing.T) {
	served := []string{
		"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
		"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex", "gpt-5.3-codex-spark",
		"gpt-5.2", "codex-auto-review",
	}
	cases := []struct{ in, want string }{
		{"gpt-6-sol", "gpt-5.6-sol"},               // a digit dropped
		{"GPT-6 Astra", "gpt-6-astra"},             // case and separator
		{"hypitoken/gpt-5.6-luna", "gpt-5.6-luna"}, // a routing prefix left on
		{"gpt-5.6-sol-high", "gpt-5.6-sol"},        // a suffix borrowed from another vendor
		{"gpt-5.5-compact", "gpt-5.5"},
		{"gpt-5.5-openai-compact", "gpt-5.5"},
		{"gpt-6-astra-codex", "gpt-6-astra"},
		{"gpt-5.5-high", "gpt-5.5"},
		{"gpt6", "gpt-6-astra"},
		{"gpt-6", "gpt-6-astra"},
	}
	for _, tc := range cases {
		if got := closestModelName(tc.in, served); got != tc.want {
			t.Errorf("closestModelName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A guess has to be worth making. Sending someone from one name that fails to
// another that also fails is worse than saying we do not know: they try it,
// wait, and come back no wiser.
func TestClosestModelNameDeclinesToGuessWildly(t *testing.T) {
	served := []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra"}
	for _, in := range []string{"claude-sonnet-4-6", "deepseek-v4-flash", "nonexistent-model-83ba36527468"} {
		if got := closestModelName(in, served); got != "" {
			t.Errorf("closestModelName(%q) = %q, want no guess — it is a different vendor's model, not a typo", in, got)
		}
	}
	if got := closestModelName("gpt-5.6-sol", nil); got != "" {
		t.Errorf("with nothing served the answer is %q, want empty", got)
	}
}

// A parenthesised variant is a real, working request — 1512 of them on
// gpt-5.6-terra(1m) alone — and the catalog carries only the base name. The
// guard checks the base or it refuses traffic that has always worked.
func TestCodexModelVariantsResolveToTheirBase(t *testing.T) {
	served := map[string]bool{
		"gpt-5.6-sol": true, "gpt-5.6-terra": true, "gpt-5.6-luna": true,
		"gpt-5.5": true, "gpt-5.4": true, "gpt-6-astra": true,
	}
	// Every one of these is real production traffic that produced output.
	// gpt-5.6-luna-max alone has 51 successful requests; refusing it because
	// no catalog spells the suffix would break working customers.
	for _, m := range []string{
		"gpt-5.6-sol(1m)", "gpt-5.6-terra(1m)", "gpt-5.6-luna(1m)",
		"gpt-5.6-luna-max", "gpt-5.6-sol-ultra", "gpt-5.6-sol-ultra-openai-compact",
		"gpt-5.5-openai-compact", "gpt-5.5-high", "gpt-5.5-low",
		"gpt-5.5-low-openai-compact", "gpt-5.4-high", "gpt-5.6-terra-high",
		"gpt-5.6-sol-low", "gpt-5.5-compact", "gpt-6-astra",
	} {
		if !codexModelServable(m, served) {
			t.Errorf("%s was refused; it is a variant of a served model and has produced real output", m)
		}
	}
	// And the trimming must not run so far that a wrong name finds a base.
	for _, m := range []string{
		"gpt-6-sol", "hypitoken/gpt-5.6-luna", "claude-sonnet-4-6",
		"5.6luna", "deepseek-chat", "gpt6", "gpt", "gpt-4",
	} {
		if codexModelServable(m, served) {
			t.Errorf("%s was allowed; nothing serves it and it has never produced output", m)
		}
	}
}

// codex-auto-review is served — 4159 requests at 93% success here — and is
// deliberately absent from the advertised catalog because the vendor CLI never
// offers it. An allowlist built from the advertised list alone refuses it.
func TestHiddenModelsAreServable(t *testing.T) {
	for _, m := range []string{"codex-auto-review", "gpt-reserve"} {
		if !codexHiddenModels[m] {
			t.Errorf("%s is served but not in the hidden-model allowlist — the guard would refuse it at the door", m)
		}
	}
}

// The suggestion must survive the normalisation both sides go through: the
// message quotes the model the customer typed, not the normalised form.
func TestUnknownModelErrorNamesBothModelAndSuggestion(t *testing.T) {
	set := map[string]bool{"gpt-5.6-sol": true, "gpt-6-astra": true}
	rej := codexUnknownModelError("gpt-6-sol", set)
	if rej.Status != 404 || rej.Code != "model_not_found" {
		t.Fatalf("status=%d code=%q, want 404/model_not_found", rej.Status, rej.Code)
	}
	if rej.Model != "gpt-6-sol" {
		t.Errorf("Model = %q, want the name as typed", rej.Model)
	}
	if rej.Suggestion != "gpt-5.6-sol" {
		t.Errorf("Suggestion = %q, want gpt-5.6-sol", rej.Suggestion)
	}
	for _, want := range []string{`"gpt-6-sol"`, `"gpt-5.6-sol"`, "/v1/models"} {
		if !contains(rej.Message, want) {
			t.Errorf("message %q is missing %q", rej.Message, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// codexGuardServer builds a pool holding exactly the given credentials, which
// is all guardCodexModel reads.
func codexGuardServer(creds ...*auth.Auth) *Server {
	return codexHTTPTestServer("https://unused.invalid", creds...)
}

// A model the subscription tier serves goes through untouched: no refusal, and
// no forced API-key detour that would strand subscription traffic.
func TestGuardPassesCatalogModels(t *testing.T) {
	s := codexGuardServer(wsCred("pro-1"))
	for _, m := range []string{"gpt-5.6-sol", "gpt-6-astra", "gpt-5.5", "codex-auto-review", "gpt-5.6-terra(1m)"} {
		route := s.guardCodexModel(m)
		if route.reject != nil {
			t.Errorf("%s was refused: %s", m, route.reject.Message)
		}
		if route.apiKeyOnly {
			t.Errorf("%s was pushed onto the API-key path, stranding it when no key is loaded", m)
		}
	}
}

// An empty or unparsed model must never be refused. forward() substitutes
// "unknown" when the body carries no model, and some routes legitimately do.
func TestGuardIgnoresAnUnnamedModel(t *testing.T) {
	s := codexGuardServer(wsCred("pro-1"))
	for _, m := range []string{"", "unknown"} {
		if route := s.guardCodexModel(m); route.reject != nil || route.apiKeyOnly {
			t.Errorf("model %q was acted on; it carries no claim to check", m)
		}
	}
}

// A subscription-only pool has a complete catalog, so an unrecognised name is
// answerable immediately — with the name and the nearest served model.
func TestGuardRefusesUnknownOnASubscriptionOnlyPool(t *testing.T) {
	s := codexGuardServer(wsCred("pro-1"))
	route := s.guardCodexModel("gpt-6-sol")
	if route.reject == nil {
		t.Fatal("gpt-6-sol was allowed through — it has never once produced output and would burn the whole pool")
	}
	if route.reject.Suggestion != "gpt-5.6-sol" {
		t.Errorf("suggestion = %q, want gpt-5.6-sol", route.reject.Suggestion)
	}
	if route.reject.Status != 404 {
		t.Errorf("status = %d, want 404", route.reject.Status)
	}
}

// The one that must never regress. An API-key credential serves models no
// catalog here enumerates, and the probe that would enumerate them is a network
// call that can be cold or failing. Refusing on a probe we have not made would
// break BYOK traffic that works — so an unfetched catalog means allow, and
// route to the key rather than parking a subscription credential on it.
func TestGuardFailsOpenWhenTheRelayCatalogIsUnknown(t *testing.T) {
	s := codexGuardServer(wsCred("pro-1"), codexAPIKeyCred("key-1"))
	route := s.guardCodexModel("some-relay-only-model")
	if route.reject != nil {
		t.Fatalf("refused %q on an unfetched relay catalog: %s", "some-relay-only-model", route.reject.Message)
	}
	if !route.apiKeyOnly {
		t.Error("the model is served by no subscription tier, so OAuth must be skipped — offering it to one is what parks the turn for the whole stall budget")
	}
}

// Once the relay catalog is known it becomes authoritative for its half: a name
// in neither catalog is refused even though an API key is loaded.
func TestGuardRefusesWhenNeitherCatalogHasIt(t *testing.T) {
	s := codexGuardServer(wsCred("pro-1"), codexAPIKeyCred("key-1"))
	s.codexModels.mu.Lock()
	s.codexModels.models = map[string]bool{"deepseek-chat": true, "MiniMax-M3": true}
	s.codexModels.fetchedAt = time.Now()
	s.codexModels.mu.Unlock()

	if route := s.guardCodexModel("deepseek-chat"); route.reject != nil || !route.apiKeyOnly {
		t.Errorf("a relay model must route to the key: reject=%v apiKeyOnly=%v", route.reject, route.apiKeyOnly)
	}
	route := s.guardCodexModel("gpt-6-sol")
	if route.reject == nil {
		t.Fatal("gpt-6-sol is in neither catalog and must be refused")
	}
	if route.reject.Suggestion != "gpt-5.6-sol" {
		t.Errorf("suggestion = %q, want gpt-5.6-sol", route.reject.Suggestion)
	}
}

// A relay that refuses probes must be asked at most once per cooldown. Without
// this the failure is self-sustaining: nothing gets cached, every request sees
// a stale entry and asks again, and the relay — which answers 401 precisely
// when probed in quick succession — keeps refusing.
func TestGuardThrottlesAFailingProbe(t *testing.T) {
	s := codexGuardServer(wsCred("pro-1"), codexAPIKeyCred("key-1"))
	_, apiKeys := s.codexOAuthModelSet()
	if len(apiKeys) != 1 {
		t.Fatalf("expected one API-key credential, got %d", len(apiKeys))
	}

	if _, ok := s.apiKeyModels(apiKeys); ok {
		t.Fatal("nothing has been fetched, so the catalog must report itself unusable")
	}
	s.codexModels.mu.Lock()
	first := s.codexModels.lastAttempt
	s.codexModels.refreshing = false // the probe goroutine's outcome is not what is under test
	s.codexModels.mu.Unlock()
	if first.IsZero() {
		t.Fatal("the first call did not record an attempt, so failures would never be throttled")
	}

	for i := 0; i < 5; i++ {
		_, _ = s.apiKeyModels(apiKeys)
	}
	s.codexModels.mu.Lock()
	again := s.codexModels.lastAttempt
	s.codexModels.mu.Unlock()
	if !again.Equal(first) {
		t.Error("a second probe was started inside the cooldown — one failing relay would be probed once per request")
	}
}

// Every API-key credential contributes, because relays do not agree: this
// deployment runs two with different catalogs, and a set built from whichever
// the pool listed first would refuse models the other serves.
func TestGuardCollectsEveryAPIKeyCredential(t *testing.T) {
	s := codexGuardServer(wsCred("pro-1"), codexAPIKeyCred("key-1"), codexAPIKeyCred("key-2"))
	_, apiKeys := s.codexOAuthModelSet()
	if len(apiKeys) != 2 {
		t.Fatalf("collected %d API-key credentials, want 2 — a partial union refuses models a relay serves", len(apiKeys))
	}
}

// The count endpoints are answered locally exactly when no credential reaches
// the vendor's own API. This is the whole decision, and getting it wrong in
// either direction is expensive: forwarding to a reseller returns the 404 that
// made count_tokens fail 1634 times in fourteen days, and estimating when the
// vendor is reachable throws away an exact answer for an approximate one.
func TestTokenCountRoutingByCredential(t *testing.T) {
	relay := func(id, provider, base string) *auth.Auth {
		//nolint:gosec // G101: fixed test fixture, not a credential.
		return &auth.Auth{ID: id, Kind: auth.KindAPIKey, Provider: provider,
			Label: id, AccessToken: "sk-relay-" + id, BaseURL: base}
	}

	// Anthropic: an OAuth credential with no override is api.anthropic.com.
	s := codexGuardServer(&auth.Auth{ID: "oauth-anthropic", Kind: auth.KindOAuth, Provider: auth.ProviderAnthropic, AccessToken: "t"})
	if !s.reachesVendorForTokenCount(auth.ProviderAnthropic) {
		t.Error("an unoverridden Anthropic OAuth credential reaches api.anthropic.com, which implements count_tokens")
	}

	// A base-URL override is a reseller, and no reseller implements the route.
	s = codexGuardServer(relay("relay-anthropic", auth.ProviderAnthropic, "https://www.duckcoding.ai"))
	if s.reachesVendorForTokenCount(auth.ProviderAnthropic) {
		t.Error("a reseller was treated as vendor-reachable — forwarding there is the 404 this exists to stop")
	}

	if s2 := codexGuardServer(relay("official-anthropic", auth.ProviderAnthropic, "https://api.anthropic.com")); !s2.reachesVendorForTokenCount(auth.ProviderAnthropic) {
		t.Error("an API key pointed at api.anthropic.com reaches the vendor route")
	}

	// OpenAI: only an API key on the vendor host. The Codex OAuth backend is
	// chatgpt.com/backend-api/codex, which has never been seen serving
	// input_tokens, so it must not be claimed.
	s = codexGuardServer(wsCred("codex-oauth"))
	if s.reachesVendorForTokenCount(auth.ProviderOpenAI) {
		t.Error("the Codex OAuth backend was claimed for input_tokens; that route is unverified there")
	}
	s = codexGuardServer(relay("openai-key", auth.ProviderOpenAI, "https://api.openai.com"))
	if !s.reachesVendorForTokenCount(auth.ProviderOpenAI) {
		t.Error("an API key on api.openai.com reaches the documented input_tokens route")
	}
	s = codexGuardServer(relay("openai-relay", auth.ProviderOpenAI, "https://api.vllmproxy.com"))
	if s.reachesVendorForTokenCount(auth.ProviderOpenAI) {
		t.Error("an OpenAI reseller was treated as vendor-reachable")
	}

	// A disabled credential admits nothing.
	dead := &auth.Auth{ID: "dead", Kind: auth.KindOAuth, Provider: auth.ProviderAnthropic, AccessToken: "t", Disabled: true}
	s = codexGuardServer(dead)
	if s.reachesVendorForTokenCount(auth.ProviderAnthropic) {
		t.Error("a disabled credential was counted as reachable")
	}
}
