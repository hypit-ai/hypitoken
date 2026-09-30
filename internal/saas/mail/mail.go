// Package mail sends transactional email.
//
// Three implementations behind the Mailer interface:
//   - ResendMailer — POSTs https://api.resend.com/emails (a single CDN-edge
//     HTTPS round-trip with a hard timeout). This is the default whenever the
//     configured host points at Resend, because the SMTP path's multi-RTT
//     handshake to AWS SES (Tokyo) is both slow and — without a timeout — prone
//     to hanging the /auth/send-code request, which strands the user mid-signup.
//     It falls back to SMTP if the API call fails.
//   - SMTPMailer — a plain SMTP client (now with dial + I/O deadlines). Used for
//     self-hosted SMTP, and as the Resend fallback.
//   - LogMailer — prints to stderr (used when nothing is configured — the
//     verification code shows up in server logs so dev can copy-paste it).
package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// smtpTimeout bounds the whole SMTP exchange (dial + TLS + AUTH + DATA). The
// previous code had no timeout at all, so a flaky HK→Tokyo hop could hang the
// request indefinitely.
const smtpTimeout = 15 * time.Second

// resendAPITimeout bounds a single Resend HTTP API call.
const resendAPITimeout = 12 * time.Second

// resendEndpoint is the transactional-send endpoint.
const resendEndpoint = "https://api.resend.com/emails"

// SMTPConfig is a local copy of SMTPConfig (mirrored to avoid an import
// cycle). Callers in the top-level saas package construct it from saas.Config.
// When Host points at Resend, Password is the Resend API key (re_…), which
// doubles as the HTTP API bearer token.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	UseTLS   bool
}

// Mailer sends one transactional message. text is the plain-text alternative;
// pass "" to send HTML-only (e.g. the shop's order mail).
type Mailer interface {
	Send(to, subject, html, text string) error
}

func New(cfg SMTPConfig, siteName string) Mailer {
	if strings.TrimSpace(cfg.Host) == "" {
		log.Warn("SMTP not configured; mailer running in log-only mode (verification codes will appear in server logs)")
		return &LogMailer{site: siteName}
	}
	smtpMailer := &SMTPMailer{cfg: cfg, site: siteName}

	// Resend HTTP API path. Requires the API key (Password) and a real From
	// address — the SMTP username is the literal "resend", not a mailbox, so we
	// can't synthesize a sender from it. Keep SMTP as the fallback.
	if strings.Contains(strings.ToLower(cfg.Host), "resend") &&
		strings.TrimSpace(cfg.Password) != "" && strings.TrimSpace(cfg.From) != "" {
		log.Infof("mailer: using Resend HTTP API (from=%s, smtp fallback enabled)", cfg.From)
		return &ResendMailer{
			apiKey:   cfg.Password,
			from:     fmt.Sprintf("%s <%s>", siteName, cfg.From),
			endpoint: resendEndpoint,
			client:   &http.Client{Timeout: resendAPITimeout},
			fallback: smtpMailer,
		}
	}
	return smtpMailer
}

// ResendMailer sends via the Resend HTTP API, falling back to SMTP on failure.
type ResendMailer struct {
	apiKey   string
	from     string // pre-formatted "Site <addr>"
	endpoint string
	client   *http.Client
	fallback Mailer
}

func (m *ResendMailer) Send(to, subject, html, text string) error {
	if err := m.sendAPI(to, subject, html, text); err != nil {
		log.Warnf("mailer: Resend API send to %s failed (%v); falling back to SMTP", to, err)
		if m.fallback != nil {
			return m.fallback.Send(to, subject, html, text)
		}
		return err
	}
	return nil
}

func (m *ResendMailer) sendAPI(to, subject, html, text string, attachments ...Attachment) error {
	body := map[string]any{
		"from":    m.from,
		"to":      []string{to},
		"subject": subject,
		"html":    html,
	}
	if strings.TrimSpace(text) != "" {
		body["text"] = text
	}
	if len(attachments) > 0 {
		body["attachments"] = attachments
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), resendAPITimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var ok struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(respBody, &ok)
		log.Infof("mailer: Resend accepted mail to=%s subject=%q id=%s", to, subject, ok.ID)
		return nil
	}
	// Surface Resend's structured error in logs only — callers map this to a
	// generic user-facing message.
	var apiErr struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(respBody, &apiErr)
	msg := strings.TrimSpace(apiErr.Message)
	if msg == "" {
		msg = strings.TrimSpace(string(respBody))
	}
	return fmt.Errorf("resend api status %d: %s", resp.StatusCode, msg)
}

type LogMailer struct {
	site string
}

func (m *LogMailer) Send(to, subject, html, text string) error {
	body := text
	if strings.TrimSpace(body) == "" {
		body = html
	}
	log.Infof("[mail-stub site=%s] to=%s subject=%q\n%s", m.site, to, subject, body)
	return nil
}

type SMTPMailer struct {
	cfg  SMTPConfig //nolint:revive
	site string
}

func (m *SMTPMailer) Send(to, subject, html, text string) error {
	from := m.cfg.From
	if from == "" {
		from = m.cfg.Username
	}
	addr := fmt.Sprintf("%s:%d", m.cfg.Host, m.cfg.Port)
	auth := smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)

	var msg strings.Builder
	fmt.Fprintf(&msg, "From: %s <%s>\r\n", m.site, from)
	fmt.Fprintf(&msg, "To: %s\r\n", to)
	fmt.Fprintf(&msg, "Subject: %s\r\n", subject)
	msg.WriteString("MIME-Version: 1.0\r\n")
	if strings.TrimSpace(text) != "" {
		// multipart/alternative: text first, HTML second (clients render the
		// last part they understand). Boundary is fixed — fine, each message
		// is self-contained.
		const boundary = "==hypitoken_alt_boundary=="
		fmt.Fprintf(&msg, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", boundary)
		fmt.Fprintf(&msg, "--%s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n", boundary, text)
		fmt.Fprintf(&msg, "--%s\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n%s\r\n", boundary, html)
		fmt.Fprintf(&msg, "--%s--\r\n", boundary)
	} else {
		msg.WriteString("Content-Type: text/html; charset=UTF-8\r\n\r\n")
		msg.WriteString(html)
	}

	if err := m.send(addr, auth, from, to, []byte(msg.String())); err != nil {
		return err
	}
	log.Infof("mailer: SMTP accepted mail to=%s subject=%q", to, subject)
	return nil
}

// send performs the full SMTP exchange with a dial timeout and a connection
// deadline so a stalled HK→SES hop can't hang the request forever. Handles both
// implicit-TLS (465) and plaintext/STARTTLS (587).
func (m *SMTPMailer) send(addr string, auth smtp.Auth, from, to string, body []byte) error {
	dialer := &net.Dialer{Timeout: smtpTimeout}
	var conn net.Conn
	var err error
	if m.cfg.UseTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: m.cfg.Host})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(smtpTimeout))

	c, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer c.Quit()

	// On a plaintext connection, upgrade to STARTTLS when offered before auth.
	if !m.cfg.UseTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: m.cfg.Host}); err != nil {
				return err
			}
		}
	}
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	return w.Close()
}

// VerificationEmail builds the subject, HTML body, and plain-text body for the
// email-verification code typed during sign-up.
func VerificationEmail(siteName, code string) (subject, html, text string) {
	subject = fmt.Sprintf("Your %s verification code: %s", siteName, code)
	intro := "Enter this code to verify your email address and finish signing in:"
	expiry := "This code expires in 10 minutes. For your security, never share it with anyone."
	html = renderShell(siteName, intro, code, expiry)
	text = renderText(siteName, intro, code, expiry)
	return
}

// ResetEmail builds the subject, HTML body, and plain-text body for the
// password-reset code typed on the forgot-password screen.
func ResetEmail(siteName, code string) (subject, html, text string) {
	subject = fmt.Sprintf("Your %s password reset code: %s", siteName, code)
	intro := "Use this code to reset your password:"
	expiry := "This code expires in 10 minutes. If you didn't request a reset, you can ignore this email — your password will stay the same."
	html = renderShell(siteName, intro, code, expiry)
	text = renderText(siteName, intro, code, expiry)
	return
}

// DatabaseAlertEmail builds an operator alert for a detected database
// integrity failure. Unlike the code mails this one is not a template shell —
// it carries a diagnostic payload and goes to the operator, not a customer.
//
// Kept deliberately plain: the recipient is being woken up, and the useful
// content is the error text plus what to do next, not layout.
func DatabaseAlertEmail(siteName, source, detail, observedAt string) (subject, html, text string) {
	subject = fmt.Sprintf("[%s] ALERT: database integrity failure", siteName)
	text = fmt.Sprintf(`%s detected SQLite corruption in the SaaS database.

Source:   %s
Observed: %s
Detail:   %s

Billing writes are failing while this persists. Recovery outline:
  1. systemctl stop hypitoken
  2. cp the live saas.db aside as evidence
  3. sqlite3 <live> ".recover" | sqlite3 <new.db>, then PRAGMA integrity_check
  4. backfill any rows .recover dropped from the newest clean snapshot in
     <dbdir>/backups/ (compare per-table counts before trusting it)
  5. swap the rebuilt file in and restart

Do not attach or write to the live database with the sqlite3 CLI while the
server is running — that corrupts it further.`, siteName, source, observedAt, detail)

	html = fmt.Sprintf(`<div style="font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:14px;line-height:1.6;color:#111">
<p style="font-size:16px"><strong>%s detected SQLite corruption in the SaaS database.</strong></p>
<table cellpadding="4" style="border-collapse:collapse">
<tr><td><strong>Source</strong></td><td>%s</td></tr>
<tr><td><strong>Observed</strong></td><td>%s</td></tr>
<tr><td><strong>Detail</strong></td><td><code>%s</code></td></tr>
</table>
<p>Billing writes are failing while this persists.</p>
<p><strong>Do not</strong> attach or write to the live database with the sqlite3 CLI while the server is running — that corrupts it further.</p>
</div>`, htmlEsc(siteName), htmlEsc(source), htmlEsc(observedAt), htmlEsc(detail))
	return
}

// DatabaseRecoveredEmail builds the all-clear that follows a successful
// self-heal, and is deliberately not just DatabaseAlertEmail with a different
// word in it. The operator has already been paged by then; what they need next
// is the two facts that decide whether they still have to get up — the file
// was intact, and billing is running again — plus the one thing the process
// cannot do for itself, which is find out who unlinked the database's files.
func DatabaseRecoveredEmail(siteName, detail, observedAt string) (subject, html, text string) {
	subject = fmt.Sprintf("[%s] RECOVERED: database self-healed", siteName)
	text = fmt.Sprintf(`%s recovered the SaaS database automatically.

Observed: %s
Detail:   %s

The connection pool was recycled and PRAGMA quick_check then passed, which
means the database FILE was never damaged — the server was holding stale file
handles. Billing writes have resumed. No recovery procedure is needed.

This almost always means something outside the server unlinked the live
saas.db-wal / saas.db-shm. The usual cause is the sqlite3 CLI being pointed at
the live database: on exit it checkpoints and removes those files, and a READ-
ONLY query does it too. Worth finding out what ran, because the process can
recover from this but cannot prevent it.

Charges attempted between the failure and this recovery were lost and can be
reconciled from the request log with:
  hypitoken reconcile-charges --from <t0> --to <t1>`, siteName, observedAt, detail)

	html = fmt.Sprintf(`<div style="font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:14px;line-height:1.6;color:#111">
<p style="font-size:16px"><strong>%s recovered the SaaS database automatically.</strong></p>
<table cellpadding="4" style="border-collapse:collapse">
<tr><td><strong>Observed</strong></td><td>%s</td></tr>
<tr><td><strong>Detail</strong></td><td><code>%s</code></td></tr>
</table>
<p>The connection pool was recycled and <code>PRAGMA quick_check</code> then passed: the database <strong>file was never damaged</strong>, the server was holding stale file handles. Billing writes have resumed and <strong>no recovery procedure is needed</strong>.</p>
<p>This almost always means something outside the server unlinked the live <code>saas.db-wal</code> / <code>saas.db-shm</code> — usually the <code>sqlite3</code> CLI pointed at the live database, which removes them on exit <strong>even for a read-only query</strong>. Worth finding out what ran.</p>
<p>Charges attempted during the outage were lost; reconcile them from the request log with <code>hypitoken reconcile-charges --from &lt;t0&gt; --to &lt;t1&gt;</code>.</p>
</div>`, htmlEsc(siteName), htmlEsc(observedAt), htmlEsc(detail))
	return
}
