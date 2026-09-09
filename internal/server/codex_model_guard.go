package server

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/pricing"
)

// Refusing, at the door, a Codex model no credential can serve.
//
// The ChatGPT backend does not reject a model name it does not recognise. It
// accepts the turn and then never schedules it, which reaches the relay as
// silence — indistinguishable from a capacity park. So the failover loop does
// the only thing it can: burn the stall budget on one credential, then the
// next, and answer 503 several minutes later for what was a typo. Production's
// long tail is nothing but typos — gpt-6-sol, gpt-6, gpt-5.6, "GPT-6 Astra",
// hypitoken/gpt-5.6-luna, gpt-5.5-compact, gpt-5.6-sol-high — each with zero
// successful requests in the entire archive, and each having cost a customer a
// four-minute wait and an error that named nothing.
//
// The fix is to answer them ourselves, immediately, naming the model and the
// nearest thing we do serve.
//
// The catalog is per credential: each OAuth credential contributes its plan
// tier (auth.CodexModelsForPlan), each API-key credential contributes whatever
// its relay advertises. Both are needed — an API key routinely serves models no
// subscription does — and the API-key half is a network call, so it is cached
// and, when unavailable, fails open. Refusing a model because a probe failed
// would be worse than the wait this exists to prevent.

// codexHiddenModels are served but deliberately absent from the advertised
// catalog. auth.CodexModelCatalog omits them because it feeds a customer-facing
// /v1/models and the vendor CLI never offers them, but they are real: this
// deployment's codex-auto-review has 4159 requests at 93% success. An allowlist
// built from the advertised catalog alone would refuse them at the door.
var codexHiddenModels = map[string]bool{
	"codex-auto-review": true,
	"gpt-reserve":       true,
}

// codexAPIKeyModelTTL bounds how stale the relay's advertised catalog may get.
// Short enough that a model added upstream becomes usable within minutes,
// long enough that the probe never sits on a request's critical path.
const codexAPIKeyModelTTL = 5 * time.Minute

// codexAPIKeyProbeMinInterval throttles the probe itself, which the TTL alone
// does not: a failed probe leaves nothing cached, so every subsequent request
// sees a stale entry and asks again. Against a relay that answers 401 under
// load — which is how these relays behave when probed in quick succession —
// that turns one failure into a probe per request and keeps it failing.
const codexAPIKeyProbeMinInterval = 30 * time.Second

// codexModelCatalog caches the API-key half of the servable set. The OAuth half
// is in-memory and recomputed per call; only this half costs a round trip.
type codexModelCatalog struct {
	mu sync.Mutex
	// models is the last complete union. Never narrowed by a failed probe, and
	// never expired: a catalog from ten minutes ago is a far better basis for
	// refusing a model than no catalog at all.
	models    map[string]bool
	fetchedAt time.Time
	// lastAttempt covers failures too, so a relay that is refusing probes is
	// asked at most once every codexAPIKeyProbeMinInterval.
	lastAttempt time.Time
	refreshing  bool
}

// codexOAuthModelSet unions what the pool's OAuth Codex credentials can serve,
// and reports whether any API-key credential is loaded for OpenAI.
func (s *Server) codexOAuthModelSet() (models map[string]bool, apiKeys []*auth.Auth) {
	models = make(map[string]bool, 16)
	for _, st := range s.pool.Status() {
		if auth.NormalizeProvider(st.Auth.Provider) != auth.ProviderOpenAI || st.Auth.Disabled {
			continue
		}
		live := s.pool.FindByID(st.Auth.ID)
		if live == nil {
			continue
		}
		if st.Auth.Kind != auth.KindOAuth {
			apiKeys = append(apiKeys, live)
			continue
		}
		_, plan := live.CodexIdentity()
		for _, m := range auth.CodexModelsForPlan(plan) {
			models[m] = true
		}
	}
	for m := range codexHiddenModels {
		models[m] = true
	}
	return models, apiKeys
}

// apiKeyModels returns the cached relay catalog, and whether it is usable. A
// stale entry triggers a background refresh and is still returned: an entry a
// few minutes old is a far better answer than blocking the request behind a
// probe. `ok` is false only when nothing has ever been fetched, which is what
// makes the guard fail open on a cold or broken probe.
func (s *Server) apiKeyModels(creds []*auth.Auth) (map[string]bool, bool) {
	s.codexModels.mu.Lock()
	models := s.codexModels.models
	stale := time.Since(s.codexModels.fetchedAt) > codexAPIKeyModelTTL
	cooled := time.Since(s.codexModels.lastAttempt) > codexAPIKeyProbeMinInterval
	if stale && cooled && !s.codexModels.refreshing {
		s.codexModels.refreshing = true
		s.codexModels.lastAttempt = time.Now()
		go s.refreshAPIKeyModels(creds)
	}
	s.codexModels.mu.Unlock()
	return models, models != nil
}

// refreshAPIKeyModels probes every API-key credential and publishes the union.
//
// Every one of them, because relays do not agree: this deployment runs two with
// different catalogs, and a set built from whichever the pool happened to list
// first would refuse models the other serves. And only when every probe
// succeeds, because a partial union is indistinguishable from a complete one
// once it is stored — the guard would then reject real models on the strength
// of a relay that was briefly unreachable. A partial answer leaves the previous
// one in place, and a first probe that fails leaves the guard open.
func (s *Server) refreshAPIKeyModels(creds []*auth.Auth) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	set := make(map[string]bool, 32)
	for _, a := range creds {
		upstream, err := s.fetchCodexAPIKeyModels(ctx, a)
		if err != nil {
			log.Warnf("codex: model-catalog probe via %s failed: %v — model guard keeps its previous answer", a.ID, err)
			s.codexModels.mu.Lock()
			s.codexModels.refreshing = false
			s.codexModels.mu.Unlock()
			return
		}
		for _, m := range upstream {
			set[m.id] = true
		}
	}

	s.codexModels.mu.Lock()
	defer s.codexModels.mu.Unlock()
	s.codexModels.refreshing = false
	s.codexModels.models = set
	s.codexModels.fetchedAt = time.Now()
	log.Infof("codex: model guard armed with %d relay models across %d API-key credentials", len(set), len(creds))
}

// codexModelRoute is what the guard decides about one requested model.
type codexModelRoute struct {
	// apiKeyOnly routes the request straight to an API-key credential. The
	// model is real but no subscription tier serves it, and handing it to an
	// OAuth credential is what parks the turn for the whole stall budget.
	apiKeyOnly bool
	// reject is set when nothing in the pool can serve the model.
	reject *codexModelReject
}

// codexModelReject is the refusal, in a shape both forks can render with their
// own error writer.
type codexModelReject struct {
	Status     int
	Code       string
	Message    string
	Model      string
	Suggestion string
}

// guardCodexModel decides how to route a requested Codex model, or refuses it.
func (s *Server) guardCodexModel(model string) codexModelRoute {
	if model == "" || model == "unknown" {
		return codexModelRoute{}
	}
	oauthSet, apiKeyCreds := s.codexOAuthModelSet()
	if codexModelServable(model, oauthSet) {
		return codexModelRoute{}
	}
	if len(apiKeyCreds) == 0 {
		// Subscription-only pool: the advertised tiers are the whole truth.
		return codexModelRoute{reject: codexUnknownModelError(model, oauthSet, nil)}
	}
	relaySet, ok := s.apiKeyModels(apiKeyCreds)
	if !ok {
		// Never probed, or every probe has failed. Fail open.
		return codexModelRoute{apiKeyOnly: true}
	}
	if codexModelServable(model, relaySet) {
		return codexModelRoute{apiKeyOnly: true}
	}
	return codexModelRoute{reject: codexUnknownModelError(model, oauthSet, relaySet)}
}

// codexModelCandidates expands a requested model into the names a catalog could
// legitimately match it under, most specific first.
//
// Two conventions ride on top of a base model and neither appears in any
// catalog. A parenthesised or bracketed variant carries the context window or
// the reasoning effort — gpt-5.6-terra(1m), gpt-5.3-codex(high) — and a
// trailing `-segment` carries the same thing spelled differently:
// gpt-5.6-luna-max, gpt-5.5-openai-compact, gpt-5.6-sol-ultra, gpt-5.4-high.
// All of them are real working traffic here; gpt-5.6-luna-max alone has 51
// successful requests. Checking only the literal name would refuse every one.
//
// The trimming rule is pricing.Catalog.Lookup's, deliberately: that is what
// already decides which price card a variant bills against, and a guard that
// disagreed with it would refuse names the biller happily charges for.
func codexModelCandidates(model string) []string {
	out := []string{model}
	seen := map[string]bool{model: true}
	add := func(m string) {
		if m = strings.TrimSpace(m); m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}

	base := strings.TrimSpace(model)
	if i := strings.IndexByte(base, '('); i > 0 {
		base = strings.TrimSpace(base[:i])
	}
	base = pricing.StripContextModeSuffix(base)
	add(base)

	for i := strings.LastIndex(base, "-"); i > 0; i = strings.LastIndex(base[:i], "-") {
		add(base[:i])
	}
	return out
}

// codexModelServable reports whether any catalog serves the model under any of
// its candidate names.
func codexModelServable(model string, sets ...map[string]bool) bool {
	for _, cand := range codexModelCandidates(model) {
		for _, set := range sets {
			if set[cand] {
				return true
			}
		}
	}
	return false
}

func codexUnknownModelError(model string, sets ...map[string]bool) *codexModelReject {
	var available []string
	seen := map[string]bool{}
	for _, set := range sets {
		for m := range set {
			if !seen[m] {
				seen[m] = true
				available = append(available, m)
			}
		}
	}
	sort.Strings(available)

	near := closestModelName(model, available)
	msg := "Unknown model " + quoteModel(model) + "."
	if near != "" {
		msg += " Did you mean " + quoteModel(near) + "?"
	}
	msg += " Call /v1/models for the models this gateway serves."
	return &codexModelReject{
		Status:     404,
		Code:       "model_not_found",
		Message:    msg,
		Model:      model,
		Suggestion: near,
	}
}

func quoteModel(m string) string { return "\"" + m + "\"" }

// closestModelName picks the served model a mistyped name most likely meant.
//
// The shapes that actually occur are not random noise, so the rules are ordered
// by how much they explain:
//
//	gpt-6-sol            → gpt-5.6-sol            (a digit dropped)
//	GPT-6 Astra          → gpt-6-astra            (case and separator)
//	hypitoken/gpt-5.6-luna → gpt-5.6-luna         (a routing prefix left on)
//	gpt-5.6-sol-high     → gpt-5.6-sol            (a suffix from another vendor)
//	gpt-6                → gpt-6-astra            (the family, not the model)
//
// An empty return means nothing was close enough to be worth guessing at;
// a wrong guess sends the user off to try a second name that also fails.
func closestModelName(want string, available []string) string {
	if len(available) == 0 {
		return ""
	}
	w := normalizeModelName(want)
	if w == "" {
		return ""
	}

	// Exact match once punctuation and case are ignored, then containment
	// either way — both are near-certainties, so they outrank distance.
	for _, pass := range []int{0, 1} {
		best, bestScore := "", 0
		for _, m := range available {
			n := normalizeModelName(m)
			switch pass {
			case 0:
				if n != w {
					continue
				}
			case 1:
				if !strings.Contains(n, w) && !strings.Contains(w, n) {
					continue
				}
			}
			// Prefer the shortest candidate that still explains the input, so
			// "gpt-5.6" resolves to a real model rather than the longest one
			// that happens to contain it. The zero value of bestScore is not a
			// valid score — every real one is negative — so the first match has
			// to be taken on `best == ""` rather than on the comparison.
			if score := -len(n); best == "" || score > bestScore {
				best, bestScore = m, score
			}
		}
		if best != "" {
			return best
		}
	}

	// Edit distance, with a budget that scales: a two-character slip in a short
	// name is a typo, the same slip in "claude-sonnet-4-6" is a different model.
	budget := len(w) / 4
	if budget < 2 {
		budget = 2
	}
	best, bestDist := "", budget+1
	for _, m := range available {
		if d := levenshtein(w, normalizeModelName(m)); d < bestDist {
			best, bestDist = m, d
		}
	}
	return best
}

// normalizeModelName reduces a model id to the characters that carry identity,
// so "GPT-6 Astra", "gpt_6_astra" and "gpt-6-astra" compare equal.
func normalizeModelName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			m := prev[j] + 1
			if v := cur[j-1] + 1; v < m {
				m = v
			}
			if v := prev[j-1] + cost; v < m {
				m = v
			}
			cur[j] = m
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
