package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wjsoj/cc-core/auth"
)

func TestManualAPIKeyEnableClearsFailuresAndPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key.json")
	data := []byte(`{"type":"openai_api_key","api_key":"test-only","disabled":true}`)
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := auth.ParseFile(p, data)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		a.MarkFailure("upstream 502")
	}
	a.MarkQuotaExceeded(time.Now().Add(time.Hour))
	h := &Handler{pool: auth.NewPool(nil, []*auth.Auth{a}, time.Minute, false, "")}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: a.ID}}
	c.Request = httptest.NewRequest(http.MethodPatch, "/auths/"+a.ID, bytes.NewBufferString(`{"disabled":false}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.handlePatchAuth(c)
	if w.Code != 200 || a.Snapshot().Disabled || !a.IsHealthy() {
		t.Fatalf("manual recovery failed: %d %s", w.Code, w.Body.String())
	}
	_, _, _, consecutive := a.HealthSnapshot()
	if consecutive != 0 {
		t.Fatal("manual enable retained old failure run")
	}
	data, err = os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := auth.ParseFile(p, data)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Snapshot().Disabled {
		t.Fatal("manual recovery not persisted")
	}
}

func TestAPIKeyAllowedModelsPatchAndClear(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key.json")
	data := []byte(`{"type":"openai_api_key","api_key":"test-only"}`)
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := auth.ParseFile(p, data)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{pool: auth.NewPool(nil, []*auth.Auth{a}, time.Minute, false, "")}
	for _, body := range []string{`{"allowed_models":["gpt-6-sol"]}`, `{"allowed_models":[]}`} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: a.ID}}
		c.Request = httptest.NewRequest(http.MethodPatch, "/auths/"+a.ID, bytes.NewBufferString(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.handlePatchAuth(c)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		data, _ = os.ReadFile(p)
		reloaded, err := auth.ParseFile(p, data)
		if err != nil {
			t.Fatal(err)
		}
		want := body == `{"allowed_models":[]}`
		if reloaded.AcceptsModel("gpt-6-luna") != want {
			t.Fatal("restriction did not persist or clear")
		}
	}
}

func TestAllowedModelsPatchRejectsOAuthBeforeAnyMutation(t *testing.T) {
	a := &auth.Auth{ID: "oauth", Kind: auth.KindOAuth, Label: "original"}
	h := &Handler{pool: auth.NewPool([]*auth.Auth{a}, nil, time.Minute, false, "")}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: a.ID}}
	c.Request = httptest.NewRequest(http.MethodPatch, "/auths/oauth", bytes.NewBufferString(`{"allowed_models":["gpt-6-sol"],"label":"changed","disabled":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.handlePatchAuth(c)
	if w.Code != 400 || a.Label != "original" || a.Snapshot().Disabled {
		t.Fatal("OAuth restriction was accepted or partially applied")
	}
}
