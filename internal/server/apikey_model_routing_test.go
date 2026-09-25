package server

import (
	"testing"
	"time"

	"github.com/wjsoj/CPA-Claude/internal/config"
	"github.com/wjsoj/cc-core/auth"
)

func TestConfiguredAPIKeyModelUsesLocalAllowlistsWithoutProbing(t *testing.T) {
	a := &auth.Auth{ID: "relay", Kind: auth.KindAPIKey, Provider: auth.ProviderOpenAI, AllowedModels: []string{"gpt-6-sol"}}
	oauth := &auth.Auth{ID: "oauth", Kind: auth.KindOAuth, Provider: auth.ProviderOpenAI}
	s := &Server{cfg: &config.Config{OpenAIAPIKeyOnlyModels: []string{"gpt-6-sol", "gpt-6-luna"}}, pool: auth.NewPool([]*auth.Auth{oauth}, []*auth.Auth{a}, time.Minute, false, "")}
	if route := s.guardCodexModel("gpt-6-sol"); !route.apiKeyOnly || route.reject != nil {
		t.Fatal("verified model did not route to API keys")
	}
	if route := s.guardCodexModel("gpt-6-luna"); route.reject == nil || route.reject.Status != 503 {
		t.Fatal("unsupported model did not fail immediately")
	}
	if !s.codexModels.lastAttempt.IsZero() {
		t.Fatal("configured model triggered a model-list probe")
	}
	a.SetAllowedModels([]string{"gpt-6-luna"})
	if route := s.guardCodexModel("gpt-6-luna"); !route.apiKeyOnly || route.reject != nil {
		t.Fatal("manual allowlist change did not take effect immediately")
	}
	a.SetDisabled(true)
	if route := s.guardCodexModel("gpt-6-luna"); route.reject == nil {
		t.Fatal("disabled channel was considered available")
	}
}
