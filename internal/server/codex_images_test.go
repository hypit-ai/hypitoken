package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wjsoj/cc-core/codeximage"
)

const imagesOK = `{"created":1,"data":[{"b64_json":"iVBORw0K"}],"output_format":"png",` +
	`"usage":{"input_tokens":16,"input_tokens_details":{"image_tokens":0,"text_tokens":16},"output_tokens":429,"total_tokens":445}}`

type imagesBackend struct {
	mu     sync.Mutex
	path   string
	body   map[string]any
	accept string
	status int
	reply  string
	delay  time.Duration
}

func (b *imagesBackend) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.path, b.accept = r.URL.Path, r.Header.Get("Accept")
		_ = json.Unmarshal(raw, &b.body)
		status, reply, delay := b.status, b.reply, b.delay
		b.mu.Unlock()
		time.Sleep(delay)
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func imagesCtx(t *testing.T, path, contentType string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", contentType)
	return c, rec
}

func TestCodexImagesGenerationRelaysAndBills(t *testing.T) {
	be := &imagesBackend{reply: imagesOK}
	cred := codexWSTestOAuth("codex-img-1")
	s := codexHTTPTestServer(be.start(t), cred)
	body := []byte(`{"prompt":"a red cube","size":"1024x1024","quality":"low","stream":false}`)
	c, rec := imagesCtx(t, codeximage.GenerationsPath, "application/json", body)
	model, _ := codeximage.Peek(body, "application/json")

	retry, done := s.doForwardCodexImages(c, cred, codeximage.GenerationsPath, body, model, "sk-user", "tester", "", time.Now(), 1)
	if retry || !done {
		t.Fatalf("retry=%v done=%v", retry, done)
	}
	if be.path != "/codex/images/generations" || be.accept != "application/json" {
		t.Fatalf("upstream path=%q accept=%q", be.path, be.accept)
	}
	if be.body["model"] != "gpt-image-2" || be.body["prompt"] != "a red cube" {
		t.Fatalf("upstream body %v", be.body)
	}
	if _, ok := be.body["stream"]; ok {
		t.Fatal("stream forwarded upstream")
	}
	if rec.Code != http.StatusOK || rec.Body.String() != imagesOK {
		t.Fatalf("client got %d %q", rec.Code, rec.Body.String())
	}
	pc := s.usage.SnapshotClients()["sk-user"]
	if pc.Total.Tokens.ImageGenOutputTokens != 429 || pc.Total.Tokens.ImageGenTextInputTokens != 16 {
		t.Fatalf("usage not recorded for client: %+v", pc.Total.Tokens)
	}
	if want := (16*5.0 + 429*30.0) / 1e6; pc.Total.CostUSD < want*0.999 || pc.Total.CostUSD > want*1.001 {
		t.Fatalf("billed %v, want gpt-image-2 card %v", pc.Total.CostUSD, want)
	}
}

func TestCodexImagesEditMultipartBecomesJSON(t *testing.T) {
	be := &imagesBackend{reply: imagesOK}
	cred := codexWSTestOAuth("codex-img-2")
	s := codexHTTPTestServer(be.start(t), cred)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", "make it blue")
	_ = mw.WriteField("n", "2")
	fw, _ := mw.CreateFormFile("image[]", "in.png")
	_, _ = fw.Write([]byte("\x89PNG\r\n\x1a\n0000"))
	mfw, _ := mw.CreateFormFile("mask", "mask.png")
	_, _ = mfw.Write([]byte("\x89PNG\r\n\x1a\n1111"))
	_ = mw.Close()
	ct := mw.FormDataContentType()
	c, rec := imagesCtx(t, codeximage.EditsPath, ct, buf.Bytes())
	model, _ := codeximage.Peek(buf.Bytes(), ct)

	s.doForwardCodexImages(c, cred, codeximage.EditsPath, buf.Bytes(), model, "sk-user", "tester", "", time.Now(), 1)
	if rec.Code != http.StatusOK || be.path != "/codex/images/edits" {
		t.Fatalf("code=%d path=%q", rec.Code, be.path)
	}
	if be.body["n"] != float64(2) || be.body["prompt"] != "make it blue" || be.body["model"] != "gpt-image-2" {
		t.Fatalf("fields: %v", be.body)
	}
	imgs, _ := be.body["images"].([]any)
	if len(imgs) != 1 || !strings.HasPrefix(imgs[0].(map[string]any)["image_url"].(string), "data:image/png;base64,") {
		t.Fatalf("images: %v", be.body["images"])
	}
	mask, _ := be.body["mask"].(map[string]any)
	if !strings.HasPrefix(mask["image_url"].(string), "data:image/png;base64,") {
		t.Fatalf("mask: %v", be.body["mask"])
	}
}

func TestCodexImagesSlowGenerationKeepsConnectionAlive(t *testing.T) {
	old := codeximage.CommitAfter
	codeximage.CommitAfter = 20 * time.Millisecond
	t.Cleanup(func() { codeximage.CommitAfter = old })

	be := &imagesBackend{reply: imagesOK, delay: 150 * time.Millisecond}
	cred := codexWSTestOAuth("codex-img-3")
	s := codexHTTPTestServer(be.start(t), cred)
	body := []byte(`{"prompt":"slow"}`)
	c, rec := imagesCtx(t, codeximage.GenerationsPath, "application/json", body)
	s.doForwardCodexImages(c, cred, codeximage.GenerationsPath, body, "gpt-image-2", "sk-user", "tester", "", time.Now(), 1)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var v map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&v); err != nil || v["data"] == nil {
		t.Fatalf("committed body not valid JSON: %v", err)
	}
}

func TestCodexImagesFailuresBeforeCommitFailOver(t *testing.T) {
	for _, tc := range []struct {
		status int
		reply  string
	}{
		{http.StatusTooManyRequests, `{"detail":"Rate limit exceeded"}`},
		{http.StatusBadGateway, `bad gateway`},
		{http.StatusNotFound, `{"detail":"Not Found"}`},
	} {
		be := &imagesBackend{status: tc.status, reply: tc.reply}
		cred := codexWSTestOAuth("codex-img-f")
		s := codexHTTPTestServer(be.start(t), cred)
		body := []byte(`{"prompt":"x"}`)
		c, rec := imagesCtx(t, codeximage.GenerationsPath, "application/json", body)
		retry, done := s.doForwardCodexImages(c, cred, codeximage.GenerationsPath, body, "gpt-image-2", "sk-user", "tester", "", time.Now(), 1)
		if !retry || done || rec.Body.Len() != 0 {
			t.Fatalf("%d: retry=%v done=%v body=%q", tc.status, retry, done, rec.Body.String())
		}
	}
}

func TestCodexImagesRejectsBadRequests(t *testing.T) {
	cred := codexWSTestOAuth("codex-img-r")
	s := codexHTTPTestServer("http://127.0.0.1:1", cred)
	for _, tc := range []struct{ model, body string }{
		{"gpt-5.6-sol", `{"prompt":"x"}`},
		{"gpt-image-2", `{"prompt":""}`},
		{"gpt-image-2", `{"prompt":"x","stream":true}`},
	} {
		c, rec := imagesCtx(t, codeximage.GenerationsPath, "application/json", []byte(tc.body))
		retry, done := s.doForwardCodexImages(c, cred, codeximage.GenerationsPath, []byte(tc.body), tc.model, "sk-user", "tester", "", time.Now(), 1)
		if retry || !done || rec.Code != http.StatusBadRequest {
			t.Fatalf("%s %s: retry=%v done=%v code=%d", tc.model, tc.body, retry, done, rec.Code)
		}
	}
}
