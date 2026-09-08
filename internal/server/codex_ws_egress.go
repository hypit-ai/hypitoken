package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wjsoj/cc-core/auth"
	"github.com/wjsoj/cc-core/codexws"
	"github.com/wjsoj/cc-core/mimicry"

	"github.com/wjsoj/CPA-Claude/internal/config"
)

// Codex WebSocket EGRESS: forwarding an ordinary HTTP request over an upstream
// WebSocket instead of the legacy HTTP POST /codex/responses.
//
// This is the opposite direction from codex_ws.go, which accepts a WebSocket
// from a client that already speaks the protocol. Almost no client does — the
// traffic is HTTP — so the ingress route reaches very little of the fleet. This
// file is what lets the majority of requests use the transport a current
// codex-tui actually uses.
//
// # Why bother
//
//  1. A WebSocket carries protocol-level ping/pong, so it survives the
//     multi-second silences (reasoning -> answer, tool thinking) that truncate
//     an idle HTTP SSE stream. Those truncations are what clients report as
//     "stream disconnected before completion".
//  2. Real codex-tui 0.135+ streams turns over the WebSocket. The HTTP POST
//     route is the LEGACY one. Presenting a current client's User-Agent,
//     version header and beta list while using the old transport is a
//     one-comparison difference from every genuine client.
//
// # Why it is cheap to add
//
// Both transports carry the same events; only the envelope differs. cc-core's
// codexws.SSEStream re-adds the `event:`/`data:` lines, so the entire response
// pipeline below this file — line reader, shed withholding, usage extraction,
// keepalive relay, the chat-completions bridge, and the non-streaming
// aggregator — consumes a WebSocket turn unchanged. Nothing downstream knows
// which transport served it.
//
// # Why it is safe to enable
//
// Every failure mode lands before the first byte reaches the client, which is
// the same window the credential failover already owns. A dial that fails, a
// pooled socket the server closed, a turn that breaks before output: all of
// them fall back to the HTTP path within the SAME attempt, so the caller sees
// latency and nothing else. Past the first byte there is no fallback, and none
// is attempted — that case is reported exactly as an HTTP stream break is.

// codexWSEgress owns the upstream WebSocket pool and the per-credential
// cooldown that keeps a refusing backend from costing every request a failed
// dial.
type codexWSEgress struct {
	cfg       config.CodexWSUpstreamConfig
	baseURL   string
	betaValue string
	readLimit int64
	useUTLS   bool

	pool *codexws.Pool

	// dialFn is codexws.Dial in production. It is a field because the real one
	// always negotiates TLS (the upstream is wss:// only), so a test that wants
	// to exercise the frame we put on the wire and the lease lifecycle around
	// it has no other way in.
	dialFn func(context.Context, codexws.DialConfig) (codexws.Conn, *http.Response, error)

	mu       sync.Mutex
	cooldown map[string]time.Time
}

// newCodexWSEgress builds the egress from config. It is always constructed and
// is inert unless codex_ws.upstream.mode selects a WebSocket mode, so the
// call sites need no nil checks.
func newCodexWSEgress(cfg *config.Config) *codexWSEgress {
	up := cfg.CodexWS.Upstream
	e := &codexWSEgress{
		cfg:       up,
		baseURL:   cfg.ChatGPTBackendBaseURL,
		betaValue: codexws.CodexOpenAIBetaWS,
		readLimit: cfg.CodexWS.ReadLimitBytes,
		useUTLS:   cfg.UseUTLS,
		cooldown:  map[string]time.Time{},
		dialFn:    codexws.Dial,
	}
	if cfg.CodexWS.BetaVersion == "v1" {
		e.betaValue = codexws.CodexOpenAIBetaWSV1
	}
	if up.WSEgressEnabled() {
		e.pool = codexws.NewPool(codexws.PoolConfig{
			IdleTTL:    up.PoolIdle(),
			MaxAge:     up.PoolMaxAge(),
			MaxEntries: up.PoolMaxEntries,
		})
		log.Infof("codex ws egress: mode=%s pool(idle=%s max_age=%s max=%d) read_timeout=%s",
			up.Mode, up.PoolIdle(), up.PoolMaxAge(), up.PoolMaxEntries, up.ReadTimeout())
	}
	return e
}

// Close releases every pooled socket. Called from Server.Shutdown.
func (e *codexWSEgress) Close() {
	if e != nil && e.pool != nil {
		e.pool.Close()
	}
}

// eligible reports whether this particular request may go over a WebSocket.
//
// The exclusions are all cases where the WebSocket is known not to be an
// equivalent of the HTTP call, rather than cases where it merely might fail —
// a might-fail case is what the fallback is for.
func (e *codexWSEgress) eligible(a *auth.Auth, path string, snapBaseURL string) bool {
	if e == nil || e.pool == nil || !e.cfg.WSEgressEnabled() {
		return false
	}
	if a == nil || a.Kind != auth.KindOAuth {
		// Only ChatGPT subscription credentials reach chatgpt.com. An API key
		// talks to api.openai.com or a reseller relay, neither of which speaks
		// this protocol.
		return false
	}
	if path == "/v1/responses/compact" {
		// Compaction is a request/response JSON call on its own backend route.
		// There is no WebSocket equivalent to forward it over.
		return false
	}
	if strings.TrimSpace(snapBaseURL) != "" {
		// A per-credential base URL override points at a vendor relay. Relays
		// resell the Responses HTTP API; assuming one terminates WebSockets is
		// how a working credential turns into a broken one.
		return false
	}
	return !e.inCooldown(a.ID)
}

func (e *codexWSEgress) inCooldown(authID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	until, ok := e.cooldown[authID]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(e.cooldown, authID)
		return false
	}
	return true
}

// noteFailure pins this credential to the HTTP path for the cooldown window.
//
// Keyed on the credential rather than the conversation because that is the
// granularity at which the cause lives: a backend that refuses WebSockets for
// an account refuses them for all of its sessions, and an egress path that
// cannot carry WebSockets at all cannot carry them for any. Over-reacting to a
// single bad conversation costs ten minutes on the path that was the only path
// until this release.
func (e *codexWSEgress) noteFailure(authID string, reason error) {
	e.mu.Lock()
	e.cooldown[authID] = time.Now().Add(e.cfg.FallbackCooldown())
	e.mu.Unlock()
	log.Warnf("codex ws egress: %s falling back to HTTP for %s: %v", authID, e.cfg.FallbackCooldown(), reason)
}

// noteSuccess clears a cooldown once the credential proves it can carry a turn.
func (e *codexWSEgress) noteSuccess(authID string) {
	e.mu.Lock()
	delete(e.cooldown, authID)
	e.mu.Unlock()
}

// codexWSBody is the synthetic response body for a WebSocket-served turn.
//
// Close is where the connection's fate is decided, which is why the lease is
// carried here rather than released by the caller: every path that finishes
// with a Codex response already closes resp.Body — the streaming relay, the
// aggregator, the shed rollbacks and the deferred close — so hanging the
// release off Close means no new call site can forget it and leak a socket.
//
// keep is granted only for a turn that reached a terminal event cleanly. A
// socket whose turn broke early may still have that turn's frames queued on it,
// and handing it to the next turn would deliver them as if they belonged to it.
//
// A REFUSED turn still qualifies. A capacity shed ends with response.failed,
// which is terminal: the turn is over upstream and nothing of it is left on the
// wire, so the socket is clean even though the caller is about to retry the
// request on a different credential. Whether the turn was served and whether the
// socket is reusable are separate questions, and conflating them would close a
// healthy connection on every shed — which is precisely the moment the account
// is busiest and a reconnect costs the most.
type codexWSBody struct {
	*codexws.SSEStream
	lease *codexws.Lease
	once  sync.Once
	on    *codexWSEgress
	auth  string
}

func (b *codexWSBody) Close() error {
	b.once.Do(func() {
		clean := b.Terminal() && b.Err() == nil
		if clean {
			b.on.noteSuccess(b.auth)
		}
		b.lease.Release(clean)
	})
	return nil
}

// codexWSTurn is a WebSocket turn dressed as an HTTP response.
type codexWSTurn struct {
	resp   *http.Response
	stream *codexws.SSEStream
	reused bool
}

// dial opens (or reuses) the upstream socket, sends the turn, and returns it as
// an *http.Response whose Body is the turn's events rendered as SSE.
//
// upstreamBody is the SAME sanitized Responses body the HTTP path would have
// POSTed. The only transformation is the envelope: a `response.create` type
// prepended, then the client_metadata and prompt_cache_key bound to this
// connection's identity so the frame and the handshake headers agree — which a
// genuine client's always do.
func (e *codexWSEgress) dial(
	ctx context.Context,
	a *auth.Auth,
	upstreamBody []byte,
	sessionID, routingModel, routingTier string,
) (*codexWSTurn, error) {
	ident, err := e.identity(a, sessionID)
	if err != nil {
		return nil, err
	}
	// The pool key carries the credential id on top of the session id. The
	// session id already embeds the account key, so two credentials for the
	// SAME ChatGPT account would otherwise share a socket — and that socket
	// holds whichever bearer happened to dial it. A credential that is later
	// disabled, rate-limited or rotated would keep serving turns through its
	// old socket, invisibly, because nothing above this line would know which
	// bearer the bytes actually went out under.
	poolKey := a.ID + "|" + sessionID

	frame, err := mimicry.NewCodexResponseCreateFrame(upstreamBody)
	if err != nil {
		return nil, fmt.Errorf("build response.create frame: %w", err)
	}
	// The backend streams a turn's events over the socket regardless of what
	// the client asked for; a non-streaming caller is served by aggregating
	// them downstream, exactly as the HTTP path already does for a relay that
	// ignores `stream:false`. Real frames always carry stream:true.
	if rewritten, serr := setJSONBool(frame, "stream", true); serr == nil {
		frame = rewritten
	}
	frame, err = mimicry.RewriteCodexClientFrame(frame, ident)
	if err != nil {
		return nil, fmt.Errorf("bind frame identity: %w", err)
	}

	accessToken, _ := a.Credentials()
	accountID, _ := a.CodexIdentity()
	header := codexws.BuildUpstreamHeadersWithOptions(codexws.UpstreamHeaderOptions{
		AccessToken: accessToken,
		AccountID:   accountID,
		Identity:    &ident,
		BetaValue:   e.betaValue,
	})
	// Derived from the bytes actually going out, for the same reason the HTTP
	// path derives it from upstreamBody: a hint naming a different model than
	// the frame is worse than no hint.
	if hint := mimicry.CodexRoutingHint(routingModel, routingTier); hint != "" {
		header[mimicry.CodexRoutingHintHeader] = []string{hint}
	}

	snap := a.Snapshot()
	wsURL := codexWSUpstreamURL(e.baseURL)
	lease, err := e.pool.Acquire(ctx, poolKey, func(dctx context.Context) (codexws.Conn, *http.Response, error) {
		conn, resp, derr := e.dialFn(dctx, codexws.DialConfig{
			URL:       wsURL,
			Header:    header,
			ProxyURL:  snap.ProxyURL,
			UseUTLS:   e.useUTLS,
			ReadLimit: e.readLimit,
		})
		if resp != nil && resp.Body != nil {
			// On a 101 gorilla hands back a NopCloser over leftover bytes (the
			// live conn is `conn`), so closing is safe either way. Headers stay
			// readable afterwards, which is what carries the x-codex-* quota
			// snapshot up to the caller.
			_ = resp.Body.Close()
		}
		return conn, resp, derr
	})
	if err != nil {
		return nil, err
	}

	if err := lease.Conn.SetWriteDeadline(time.Now().Add(codexWSWriteDeadline)); err != nil {
		lease.Release(false)
		return nil, err
	}
	if err := lease.Conn.WriteMessage(codexws.TextMessage, frame); err != nil {
		lease.Release(false)
		return nil, fmt.Errorf("write response.create: %w", err)
	}

	stream := codexws.NewSSEStream(lease.Conn, codexws.SSEStreamOptions{ReadTimeout: e.cfg.ReadTimeout()})
	return &codexWSTurn{
		resp: &http.Response{
			Status:     "200 OK",
			StatusCode: http.StatusOK,
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     handshakeResponseHeaders(lease.Handshake),
			Body:       &codexWSBody{SSEStream: stream, lease: lease, on: e, auth: a.ID},
		},
		stream: stream,
		reused: lease.Reused,
	}, nil
}

// identity resolves the upstream session identity for this conversation.
//
// The session id comes from the same registry the HTTP path uses, anchored on
// the same string, so a conversation keeps ONE upstream session id whichever
// transport carries it. That matters more than it looks: the id is the
// backend's prompt-cache namespace, so a transport switch mid-conversation must
// not rotate it — a turn that moved from HTTP to WebSocket would otherwise
// arrive cache-cold.
func (e *codexWSEgress) identity(a *auth.Auth, sessionID string) (mimicry.CodexFrameIdentity, error) {
	id := mimicry.CodexFrameIdentity{AccountKey: a.AccountKey(), SessionID: sessionID}
	return id.Normalized()
}

// handshakeResponseHeaders returns the upstream 101's headers with the
// WebSocket-negotiation ones removed.
//
// What is kept is the x-codex-* quota snapshot and the request/trace ids, which
// the caller reads exactly as it reads them off an HTTP response. What is
// dropped are the four headers that describe an upgrade that already happened;
// they mean nothing to a caller that believes it is holding an HTTP response,
// and Sec-WebSocket-Accept in particular would be nonsense to forward.
func handshakeResponseHeaders(resp *http.Response) http.Header {
	out := http.Header{}
	if resp == nil {
		return out
	}
	for k, v := range resp.Header {
		switch strings.ToLower(k) {
		case "upgrade", "connection", "sec-websocket-accept", "sec-websocket-extensions", "sec-websocket-protocol":
			continue
		}
		out[k] = append([]string(nil), v...)
	}
	return out
}
