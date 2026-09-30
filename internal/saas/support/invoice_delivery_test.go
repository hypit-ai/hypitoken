package support

import (
	"bytes"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	saasauth "github.com/wjsoj/CPA-Claude/internal/saas/auth"
	"github.com/wjsoj/CPA-Claude/internal/saas/db"
	"github.com/wjsoj/CPA-Claude/internal/saas/mail"
)

type invoiceSender struct {
	err                     error
	calls                   int
	to, subject, html, text string
	attachments             []mail.Attachment
}

func (m *invoiceSender) SendAttachments(to, subject, html, text string, attachments []mail.Attachment) error {
	m.calls++
	m.to, m.subject, m.html, m.text, m.attachments = to, subject, html, text, attachments
	return m.err
}

func invoiceUpload(t *testing.T, email, filename string, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("email", email); err != nil {
		t.Fatal(err)
	}
	if filename != "" {
		part, err := w.CreateFormFile("pdf", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/invoices/send", &body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}

func invoiceAdminRouter(s *Service, role string) *gin.Engine {
	r := gin.New()
	g := r.Group("/admin")
	g.Use(func(c *gin.Context) {
		if role != "" {
			c.Set(string(saasauth.CtxUser), &db.User{Role: role})
		}
		c.Next()
	}, saasauth.RequireAdmin())
	s.AdminRoutes(g)
	return r
}

func TestSendInvoiceValidationAndProviderResult(t *testing.T) {
	pdf := []byte("%PDF-1.7\ninvoice\n%%EOF")
	cases := []struct {
		name, email, filename string
		data                  []byte
		err                   error
		disabled              bool
		want                  int
		calls                 int
	}{
		{name: "success", email: " customer@example.com ", filename: "发票.pdf", data: pdf, want: 200, calls: 1},
		{name: "invalid email", email: "invalid", filename: "invoice.pdf", data: pdf, want: 400},
		{name: "multiple recipients", email: "a@example.com,b@example.com", filename: "invoice.pdf", data: pdf, want: 400},
		{name: "header injection", email: "a@example.com\r\nBcc: b@example.com", filename: "invoice.pdf", data: pdf, want: 400},
		{name: "missing file", email: "customer@example.com", want: 400},
		{name: "fake pdf", email: "customer@example.com", filename: "invoice.pdf", data: []byte("plain text"), want: 400},
		{name: "wrong extension", email: "customer@example.com", filename: "invoice.exe", data: pdf, want: 400},
		{name: "oversized pdf", email: "customer@example.com", filename: "invoice.pdf", data: append(pdf, make([]byte, maxInvoicePDFSize)...), want: 413},
		{name: "oversized request", email: "customer@example.com", filename: "invoice.pdf", data: append(pdf, make([]byte, maxInvoicePDFSize+(64<<10))...), want: 413},
		{name: "provider failure", email: "customer@example.com", filename: "invoice.pdf", data: pdf, err: errors.New("provider rejected"), want: 502, calls: 1},
		{name: "unconfigured", email: "customer@example.com", filename: "invoice.pdf", data: pdf, disabled: true, want: 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &invoiceSender{err: tc.err}
			svc := New(nil, "Test", "")
			if !tc.disabled {
				svc.ConfigureInvoiceDelivery(sender)
			}
			r := invoiceAdminRouter(svc, db.RoleAdmin)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, invoiceUpload(t, tc.email, tc.filename, tc.data))
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if sender.calls != tc.calls {
				t.Fatalf("mail calls=%d want=%d", sender.calls, tc.calls)
			}
			if tc.want == 200 {
				if !strings.Contains(w.Body.String(), `"email_sent":true`) || sender.to != strings.TrimSpace(tc.email) {
					t.Fatal("missing confirmed success or wrong email")
				}
				if !strings.Contains(sender.subject, "hypitoken") || !strings.Contains(sender.html, "hypitoken") || !strings.Contains(sender.text, "hypitoken") {
					t.Fatal("wrong email brand")
				}
				if len(sender.attachments) != 1 || sender.attachments[0].Filename != tc.filename || !bytes.Equal(sender.attachments[0].Content, pdf) {
					t.Fatal("wrong attachment")
				}
			} else if strings.Contains(w.Body.String(), `"email_sent":true`) {
				t.Fatal("false success")
			}
		})
	}
}

func TestSendInvoiceRequiresAdmin(t *testing.T) {
	for _, tc := range []struct {
		role string
		want int
	}{{"", 401}, {db.RoleUser, 403}} {
		sender := &invoiceSender{}
		s := New(nil, "Test", "")
		s.ConfigureInvoiceDelivery(sender)
		w := httptest.NewRecorder()
		invoiceAdminRouter(s, tc.role).ServeHTTP(w, invoiceUpload(t, "a@example.com", "invoice.pdf", []byte("%PDF-1.7")))
		if w.Code != tc.want || sender.calls != 0 {
			t.Fatal("non-admin invoice delivery permitted")
		}
	}
}
