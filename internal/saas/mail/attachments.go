package mail

import "strings"

// Attachment is encoded as base64 by JSON when sent through Resend.
type Attachment struct {
	Filename    string `json:"filename"`
	Content     []byte `json:"content"`
	ContentType string `json:"content_type,omitempty"`
}

// AttachmentMailer is separate from Mailer so log-only development mail
// cannot silently discard invoice attachments.
type AttachmentMailer interface {
	SendAttachments(to, subject, html, text string, attachments []Attachment) error
}

// NewInvoiceMailer reuses the Resend API key and verified sender from the
// existing SMTP configuration, but sends invoices only through the HTTP API.
// nil means sending is unavailable, never a successful log-only delivery.
func NewInvoiceMailer(cfg SMTPConfig) AttachmentMailer {
	if !strings.Contains(strings.ToLower(cfg.Host), "resend") ||
		strings.TrimSpace(cfg.Password) == "" || strings.TrimSpace(cfg.From) == "" {
		return nil
	}
	return New(cfg, "hypitoken").(*ResendMailer)
}

func (m *ResendMailer) SendAttachments(to, subject, html, text string, attachments []Attachment) error {
	// Invoices must use Resend API; do not fall back to SMTP after an error.
	return m.sendAPI(to, subject, html, text, attachments...)
}
