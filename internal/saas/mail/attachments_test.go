package mail

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInvoiceAttachmentSentThroughResendAPI(t *testing.T) {
	pdf := []byte("%PDF-1.7\ninvoice\x00\xff")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			From        string       `json:"from"`
			To          []string     `json:"to"`
			Subject     string       `json:"subject"`
			HTML        string       `json:"html"`
			Text        string       `json:"text"`
			Attachments []Attachment `json:"attachments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer re_test_key" {
			t.Errorf("invalid Resend request")
		}
		if body.From != "hypitoken <no-reply@example.com>" || len(body.To) != 1 || body.To[0] != "customer@example.com" {
			t.Errorf("unexpected sender or recipient: %+v", body)
		}
		if len(body.Attachments) != 1 || body.Attachments[0].Filename != "发票.pdf" ||
			body.Attachments[0].ContentType != "application/pdf" || !bytes.Equal(body.Attachments[0].Content, pdf) {
			t.Errorf("attachment corrupted: %+v", body.Attachments)
		}
		if !strings.Contains(body.Subject, "hypitoken") || body.HTML == "" || body.Text == "" {
			t.Error("missing email copy")
		}
		_, _ = w.Write([]byte(`{"id":"invoice_email"}`))
	}))
	defer srv.Close()
	m := newTestResend(srv.URL, nil)
	m.from = "hypitoken <no-reply@example.com>"
	if err := m.SendAttachments("customer@example.com", "hypitoken · 您的发票", "<p>请查收附件</p>", "请查收附件",
		[]Attachment{{Filename: "发票.pdf", Content: pdf, ContentType: "application/pdf"}}); err != nil {
		t.Fatal(err)
	}
}

func TestInvoiceAPIErrorDoesNotFallBackToSMTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"quota exceeded"}`))
	}))
	defer srv.Close()
	fallback := &fakeMailer{}
	m := newTestResend(srv.URL, fallback)
	if err := m.SendAttachments("customer@example.com", "invoice", "body", "body", []Attachment{{Filename: "invoice.pdf", Content: []byte("%PDF-1.7")}}); err == nil {
		t.Fatal("provider rejection must return an error")
	}
	if fallback.called {
		t.Fatal("invoice must not fall back")
	}
}

func TestNewInvoiceMailerUsesHypitokenBrandAndRequiresResend(t *testing.T) {
	for _, cfg := range []SMTPConfig{{}, {Host: "mail.example.com", Password: "dummy", From: "a@example.com"}, {Host: "smtp.resend.com", From: "a@example.com"}, {Host: "smtp.resend.com", Password: "dummy"}} {
		if NewInvoiceMailer(cfg) != nil {
			t.Fatal("unconfigured Resend must disable invoice delivery")
		}
	}
	m := NewInvoiceMailer(SMTPConfig{Host: "smtp.resend.com", Password: "re_" + "dummy", From: "no-reply@example.com"}).(*ResendMailer)
	if m.from != "hypitoken <no-reply@example.com>" || m.endpoint != resendEndpoint {
		t.Fatal("invoice must use hypitoken sender through Resend API")
	}
}
