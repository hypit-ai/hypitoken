package gptpay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/checkout"
)

type backend interface {
	Create(context.Context, checkout.Auth, checkout.Selection) (checkout.Session, error)
	Quote(context.Context, checkout.Auth, checkout.Session, checkout.Selection, checkout.Billing) (checkout.Quote, error)
	Pay(context.Context, checkout.Auth, checkout.Quote, checkout.Card, *checkout.Ledger) (checkout.PaymentResult, error)
	Status(context.Context, checkout.Auth, checkout.Session) (checkout.Snapshot, error)
	Subscription(context.Context, checkout.Auth) (*auth.CodexSubscriptionInfo, error)
}
type flow struct {
	mu        sync.Mutex
	owner     string
	created   time.Time
	session   checkout.Session
	selection checkout.Selection
	quote     *checkout.Quote
	attempted bool
	proxy     string
	proxyHash string
}
type Service struct {
	origin  string
	backend backend
	ledger  *checkout.Ledger
	mu      sync.Mutex
	flows   map[string]*flow
	slots   chan struct{}
	rates   map[string]*rateWindow
}

// Visitors choose optional public SOCKS5 endpoints. Empty means direct; proxy
// failures never fall back. A flow cannot switch its selected network path.
func ServiceFromEnv() (*Service, error) {
	if os.Getenv("GPTPAY_ENABLED") != "true" {
		return nil, nil
	}
	origin := os.Getenv("GPTPAY_ORIGIN")
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("GPTPAY_ORIGIN 必须为完整 HTTPS 来源，不带路径")
	}
	ledger, err := checkout.NewLedger(os.Getenv("GPTPAY_STATE_DIR"))
	if err != nil {
		return nil, err
	}
	return &Service{origin: origin, ledger: ledger, flows: make(map[string]*flow), slots: make(chan struct{}, 8)}, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

type apiRequest struct {
	Proxy     string             `json:"proxy"`
	Session   string             `json:"session"`
	Flow      string             `json:"flow"`
	Selection checkout.Selection `json:"selection"`
	Billing   checkout.Billing   `json:"billing"`
	Card      checkout.Card      `json:"card"`
	Amount    int64              `json:"amount_minor"`
	Currency  string             `json:"currency"`
	Confirm   bool               `json:"confirm"`
	Checkout  checkout.Session   `json:"checkout"`
}

func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/config" && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"enabled": s != nil, "proxy_required": false, "proxy_mode": "optional", "default_network": "direct"})
		return
	}
	switch r.URL.Path {
	case "/api/create", "/api/quote", "/api/pay", "/api/status", "/api/recover", "/api/subscription":
	default:
		http.NotFound(w, r)
		return
	}
	if s == nil {
		fail(w, 503, "充值服务未启用")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, 405, "只接受 POST")
		return
	}
	if r.Header.Get("Origin") != s.origin || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		fail(w, 403, "请求来源不允许")
		return
	}
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		fail(w, 415, "只接受 JSON")
		return
	}
	if !s.allow(r) {
		w.Header().Set("Retry-After", "60")
		fail(w, 429, "请求过于频繁，请一分钟后再试")
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		fail(w, 429, "请求较多，请稍后查询")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128*1024)
	defer r.Body.Close()
	var req apiRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		fail(w, 400, "请求格式无效或内容过长")
		return
	}
	auth, err := checkout.ParseAuth(req.Session)
	req.Session = ""
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if r.URL.Path == "/api/recover" {
		if s.ledger == nil || !s.ledger.Owns(auth, req.Checkout) {
			fail(w, 403, "没有属于该登录态的付款记录；请核对原 Session 和结账编号")
			return
		}
		p, err := pinProxy(ctx, req.Proxy, net.DefaultResolver.LookupNetIP)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		client, closeClient, err := s.client(p)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		defer closeClient()
		snap, err := client.Status(ctx, auth, req.Checkout)
		if err != nil {
			fail(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, statusView(snap))
		return
	}
	if r.URL.Path == "/api/subscription" {
		// Read-only, no flow — a visitor should see this before committing to
		// creating a checkout session at all, so it cannot depend on one.
		p, err := pinProxy(ctx, req.Proxy, net.DefaultResolver.LookupNetIP)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		client, closeClient, err := s.client(p)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		defer closeClient()
		info, err := client.Subscription(ctx, auth)
		if err != nil {
			fail(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, subscriptionView(info))
		return
	}
	if r.URL.Path == "/api/create" {
		if err = req.Selection.Validate(); err != nil {
			fail(w, 400, err.Error())
			return
		}
		if err = req.Billing.Validate(req.Selection.Country); err != nil {
			fail(w, 400, err.Error())
			return
		}
		p, err := pinProxy(ctx, req.Proxy, net.DefaultResolver.LookupNetIP)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		f := &flow{owner: checkout.Owner(auth), created: time.Now(), selection: req.Selection, proxy: p, proxyHash: proxyHash(req.Proxy)}
		client, closeClient, err := s.client(p)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		defer closeClient()
		var random [32]byte
		if _, err = rand.Read(random[:]); err != nil {
			fail(w, 500, "暂时无法创建会话")
			return
		}
		id := hex.EncodeToString(random[:])
		s.mu.Lock()
		for k, v := range s.flows {
			if time.Since(v.created) > time.Hour {
				delete(s.flows, k)
			}
		}
		if len(s.flows) >= 256 {
			s.mu.Unlock()
			fail(w, 429, "待处理会话过多，请稍后再试")
			return
		}
		s.flows[id] = f
		s.mu.Unlock()
		time.AfterFunc(time.Hour, func() { s.mu.Lock(); delete(s.flows, id); s.mu.Unlock() })
		f.mu.Lock()
		defer f.mu.Unlock()
		f.session, err = client.Create(ctx, auth, req.Selection)
		if err != nil {
			s.mu.Lock()
			delete(s.flows, id)
			s.mu.Unlock()
			fail(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"flow": id, "checkout": f.session})
		return
	}
	s.mu.Lock()
	f := s.flows[req.Flow]
	s.mu.Unlock()
	if f == nil || checkout.Owner(auth) != f.owner || time.Since(f.created) > time.Hour {
		fail(w, 403, "会话已过期或登录态不匹配；已付款请使用结果恢复查询")
		return
	}
	if !f.mu.TryLock() {
		fail(w, 409, "该会话正在处理，请稍后查询")
		return
	}
	defer f.mu.Unlock()
	if f.proxyHash != proxyHash(req.Proxy) {
		fail(w, 409, "请保持创建结账时的代理配置；修改网络需重新创建结账")
		return
	}
	client, closeClient, err := s.client(f.proxy)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	defer closeClient()
	switch r.URL.Path {
	case "/api/quote":
		if f.attempted {
			fail(w, 409, "已尝试支付，只可查询结果")
			return
		}
		q, err := client.Quote(ctx, auth, f.session, f.selection, req.Billing)
		if err != nil {
			f.quote = nil
			fail(w, 502, err.Error())
			return
		}
		f.quote = &q
		writeJSON(w, 200, map[string]any{"amount_minor": q.Amount, "currency": q.Selection.Currency, "plan": q.Selection.Plan, "account": q.UserID, "expires_at": q.Expires.Unix()})
	case "/api/pay":
		if f.attempted {
			fail(w, 409, "已提交付款；不要重付，请查询结果")
			return
		}
		if f.quote == nil || !req.Confirm || req.Amount != f.quote.Amount || req.Currency != f.quote.Selection.Currency || !time.Now().Before(f.quote.Expires) {
			fail(w, 409, "请先获取报价并明确确认金额")
			return
		}
		if err = req.Card.Validate(); err != nil {
			fail(w, 400, err.Error())
			return
		}
		// One UI attempt even on uncertainty. The core additionally persists its
		// guard BEFORE charge-capable requests for crash/cross-process protection.
		f.attempted = true
		result, err := client.Pay(ctx, auth, *f.quote, req.Card, s.ledger)
		req.Card = checkout.Card{}
		f.quote = nil
		if err != nil {
			fail(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, result)
	case "/api/status":
		snap, err := client.Status(ctx, auth, f.session)
		if err != nil {
			fail(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, statusView(snap))
	}
}

// subscriptionSummary is deliberately a subset of auth.CodexSubscriptionInfo:
// has-ever-paid / has-active / plan / renewal / delinquency answer exactly
// what an operator checking a session before spending a real card needs to
// see. PaymentMethods (card brand + last four) is never forwarded — nothing
// here asked for "which card is on file", and echoing it back to the visitor
// who just supplied the session only widens what this page exposes for no
// requested benefit.
type subscriptionSummary struct {
	HasPreviouslyPaid bool   `json:"has_previously_paid"`
	HasActive         bool   `json:"has_active"`
	Plan              string `json:"plan,omitempty"`
	PlanNormalized    string `json:"plan_normalized"`
	WillRenew         bool   `json:"will_renew"`
	IsDelinquent      bool   `json:"is_delinquent"`
	ActiveUntil       int64  `json:"active_until,omitempty"`
}

func subscriptionView(info *auth.CodexSubscriptionInfo) subscriptionSummary {
	v := subscriptionSummary{}
	if info == nil {
		return v
	}
	if info.Account != nil {
		v.HasPreviouslyPaid = info.Account.HasPreviouslyPaidSubscription
	}
	if info.Entitlement != nil {
		v.HasActive = info.Entitlement.HasActiveSubscription
		v.IsDelinquent = info.Entitlement.IsDelinquent
	}
	// Same precedence FetchCodexSubscription itself uses to backfill a stored
	// credential's PlanType — kept identical so "the plan" means one thing
	// everywhere in this codebase, not a second opinion gptpay invented.
	if info.Portal != nil && info.Portal.PlanType != "" {
		v.Plan = info.Portal.PlanType
	} else if info.Account != nil && info.Account.PlanType != "" {
		v.Plan = info.Account.PlanType
	}
	v.PlanNormalized = auth.NormalizeCodexPlan(v.Plan)
	if info.Portal != nil {
		v.WillRenew = info.Portal.WillRenew
		v.IsDelinquent = v.IsDelinquent || info.Portal.IsDelinquent
		if !info.Portal.ActiveUntil.IsZero() {
			v.ActiveUntil = info.Portal.ActiveUntil.Unix()
		}
	}
	return v
}

func statusView(s checkout.Snapshot) checkout.PaymentResult {
	state := "pending"
	if s.Paid() {
		state = "paid"
	} else if s.Status == "expired" || s.Status == "canceled" {
		state = s.Status
	}
	return checkout.PaymentResult{State: state, Paid: s.Paid()}
}
