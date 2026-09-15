package gptpay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wjsoj/cc-core/checkout"
)

type fakeBackend struct {
	creates, pays int
	state         string
}

func (b *fakeBackend) Create(_ context.Context, _ checkout.Auth, _ checkout.Selection) (checkout.Session, error) {
	b.creates++
	return checkout.Session{ID: "oaics_fixture", Entity: "openai_llc"}, nil
}
func (b *fakeBackend) Quote(_ context.Context, _ checkout.Auth, s checkout.Session, sel checkout.Selection, bill checkout.Billing) (checkout.Quote, error) {
	return checkout.Quote{Session: s, Selection: sel, Billing: bill, Amount: 2000, Expires: time.Now().Add(time.Minute), UserID: "user_fixture"}, nil
}
func (b *fakeBackend) Pay(_ context.Context, _ checkout.Auth, _ checkout.Quote, _ checkout.Card, _ *checkout.Ledger) (checkout.PaymentResult, error) {
	b.pays++
	return checkout.PaymentResult{State: b.state, Paid: b.state == "paid"}, nil
}
func (b *fakeBackend) Status(_ context.Context, _ checkout.Auth, _ checkout.Session) (checkout.Snapshot, error) {
	if b.state == "paid" {
		return checkout.Snapshot{Status: "complete", PaymentStatus: "paid"}, nil
	}
	return checkout.Snapshot{Status: "open", PaymentStatus: "unpaid"}, nil
}
func newTestService(t *testing.T) (*Service, *fakeBackend) {
	t.Helper()
	b := &fakeBackend{state: "paid"}
	l, err := checkout.NewLedger(filepath.Join(t.TempDir(), "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	return &Service{origin: "https://gptpay.example", backend: b, ledger: l, flows: make(map[string]*flow), slots: make(chan struct{}, 8)}, b
}
func call(t *testing.T, h http.Handler, path string, body any, origin string) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
	r.Header.Set("Origin", origin)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	if json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("invalid JSON: %s", w.Body.String())
	}
	return w.Code, out
}
func requestFixture() apiRequest {
	return apiRequest{Session: "fixture_access_token_not_real_12345", Selection: checkout.Selection{Plan: "chatgptplusplan", Country: "US", Currency: "USD"}, Billing: checkout.Billing{Name: "Test", Email: "test@example.invalid", Line1: "Test", City: "Test", PostalCode: "00000", Country: "US"}}
}
func TestAPIFlowAndConfirmation(t *testing.T) {
	s, b := newTestService(t)
	h := NewHandler(s)
	req := requestFixture()
	code, out := call(t, h, "/api/create", req, s.origin)
	if code != 200 || b.creates != 1 {
		t.Fatalf("create %d %v", code, out)
	}
	req.Flow = out["flow"].(string)
	code, _ = call(t, h, "/api/pay", req, s.origin)
	if code != 409 || b.pays != 0 {
		t.Fatal("pay accepted without quote")
	}
	code, out = call(t, h, "/api/quote", req, s.origin)
	if code != 200 || out["amount_minor"] != float64(2000) {
		t.Fatalf("quote %d %v", code, out)
	}
	req.Amount = 2001
	req.Currency = "USD"
	req.Confirm = true
	req.Card = checkout.Card{Number: "4242424242424242", Month: "12", Year: "2035", CVC: "123"}
	code, _ = call(t, h, "/api/pay", req, s.origin)
	if code != 409 || b.pays != 0 {
		t.Fatal("tampered quote accepted")
	}
	req.Amount = 2000
	code, out = call(t, h, "/api/pay", req, s.origin)
	if code != 200 || out["paid"] != true || b.pays != 1 {
		t.Fatalf("pay %d %v", code, out)
	}
	code, _ = call(t, h, "/api/pay", req, s.origin)
	if code != 409 || b.pays != 1 {
		t.Fatal("duplicate pay")
	}
	code, out = call(t, h, "/api/status", req, s.origin)
	if code != 200 || out["paid"] != true {
		t.Fatal("status incorrect")
	}
	req.Session = "different_access_token_123456789"
	code, _ = call(t, h, "/api/status", req, s.origin)
	if code != 403 {
		t.Fatal("cross-owner access")
	}
}
func TestAPIRejectsUnsafeRequests(t *testing.T) {
	s, b := newTestService(t)
	h := NewHandler(s)
	req := requestFixture()
	for _, origin := range []string{"", "https://evil.example", "https://gptpay.example.evil"} {
		code, _ := call(t, h, "/api/create", req, origin)
		if code != 403 {
			t.Fatal("bad origin accepted")
		}
	}
	code, _ := call(t, h, "/api/create", map[string]any{"session": req.Session, "proxy": "socks5://internal:1234"}, s.origin)
	if code != 400 {
		t.Fatal("visitor proxy override accepted")
	}
	req.Session = strings.Repeat("x", 140000)
	code, _ = call(t, h, "/api/create", req, s.origin)
	if code != 400 {
		t.Fatal("oversized input accepted")
	}
	if b.creates != 0 || b.pays != 0 {
		t.Fatal("unsafe request reached upstream")
	}
	code, _ = call(t, NewHandler(), "/api/create", requestFixture(), s.origin)
	if code != 503 {
		t.Fatal("preview made payment available")
	}
}
func TestServiceRequiresExplicitConfiguration(t *testing.T) {
	t.Setenv("GPTPAY_ENABLED", "")
	s, err := ServiceFromEnv()
	if err != nil || s != nil {
		t.Fatal("default enabled")
	}
	t.Setenv("GPTPAY_ENABLED", "true")
	t.Setenv("GPTPAY_ORIGIN", "https://gptpay.example")
	t.Setenv("GPTPAY_STATE_DIR", "")
	if _, err = ServiceFromEnv(); err == nil {
		t.Fatal("missing persistent state directory accepted")
	}
	t.Setenv("GPTPAY_STATE_DIR", filepath.Join(t.TempDir(), "ledger"))
	s, err = ServiceFromEnv()
	if err != nil || s == nil {
		t.Fatal(err)
	}
}
