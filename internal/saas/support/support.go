// Package support implements the customer support desk: tickets from signed-in
// users, appeals from accounts that can no longer sign in, and the operator side
// that answers both.
//
// It exists because enforcement in this product is probabilistic. The signup
// anti-abuse (internal/saas/growth) withholds bonuses on evidence like a shared
// /24 or a missing browser fingerprint, and an operator acting on that evidence
// disables accounts in bulk. Some of those accounts belong to real people: a
// campus network, a privacy-conscious browser, three colleagues on one company
// domain. An appeal path is the correction mechanism that makes aggressive
// anti-abuse defensible in the first place.
//
// The design constraint that shapes everything here: a disabled user cannot
// authenticate. saasauth.RequireUser 403s them before any authed route runs, so
// the appeal channel must work with no session at all. It does that with an
// emailed OTP (proving the address is theirs) plus an access_key returned once
// at submission (a bearer capability for reading the thread back). Neither
// grants any access beyond the one ticket.
//
// Self-contained in the spirit of the other SaaS modules: it owns support_tickets
// and ticket_messages (migration v18) and couples outward only through *db.DB and
// the Mailer interface.
package support

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/wjsoj/CPA-Claude/internal/saas/db"
	"github.com/wjsoj/CPA-Claude/internal/saas/mail"
)

// Ticket kinds.
const (
	KindSupport = "support" // ordinary question from a signed-in user
	KindAppeal  = "appeal"  // "my account was disabled" — may have no session
	// KindInvoice is declared in invoice.go alongside the rest of that flow.
)

// Ticket statuses. open/pending are both live; the two terminal states are kept
// distinct so the operator panel can show what was actually decided.
const (
	StatusOpen     = "open"     // awaiting an operator reply
	StatusPending  = "pending"  // operator replied, awaiting the user
	StatusResolved = "resolved" // closed in the user's favour / question answered
	StatusRejected = "rejected" // appeal reviewed and declined
)

// Message authors.
const (
	AuthorUser  = "user"
	AuthorAdmin = "admin"
)

var (
	ErrNotFound  = errors.New("ticket not found")
	ErrForbidden = errors.New("not your ticket")
	ErrClosed    = errors.New("ticket is closed")
)

// Service is the support desk. Construct with New and hold one instance; all
// state lives in SQLite so it is safe for concurrent use.
//
// The desk stays entirely in-app. Only the separate admin invoice delivery
// endpoint uses outbound mail; appeals never depend on the mail provider.
type Service struct {
	DB       *db.DB
	SiteName string
	SiteURL  string

	codeRL *rateLimiter

	// Invoicing (invoice.go). titleSuggestURL and payment default to the
	// built-ins; ConfigureInvoicing overrides them from operator config.
	titleSuggestURL string
	payment         PaymentInfo
	httpClient      *http.Client
	invoiceMailer   mail.AttachmentMailer
}

func New(store *db.DB, siteName, siteURL string) *Service {
	return &Service{
		DB: store, SiteName: siteName, SiteURL: siteURL,
		codeRL:          newRateLimiter(),
		titleSuggestURL: defaultTitleSuggestURL,
		payment:         defaultPaymentInfo(),
		// A short client bounds the company lookup: it sits in front of a
		// customer typing, and a hung upstream must not hold the request.
		httpClient: &http.Client{Timeout: 6 * time.Second},
	}
}

// Ticket is one support thread.
type Ticket struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	Email     string    `json:"email"`
	Kind      string    `json:"kind"`
	Subject   string    `json:"subject"`
	Status    string    `json:"status"`
	LastActor string    `json:"last_actor"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// AccessKey is returned ONLY in the response that creates an anonymous
	// appeal — it is the submitter's sole handle on the thread afterwards. It is
	// never included in list responses or in the operator view.
	AccessKey string `json:"access_key,omitempty"`
	// Meta is an opaque JSON blob owned by whatever created the ticket — the
	// invoice flow puts the 抬头 in here so the operator panel can render it as
	// fields to copy rather than prose to re-read. Empty for plain tickets.
	Meta string `json:"meta,omitempty"`
	// Messages is populated on single-ticket reads.
	Messages []Message `json:"messages,omitempty"`
}

// Message is one turn in a ticket thread.
type Message struct {
	ID        int64     `json:"id"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

const ticketCols = `id, user_id, email, kind, subject, status, last_actor, meta, created_at, updated_at`

func scanTicket(row interface{ Scan(...any) error }) (*Ticket, error) {
	var t Ticket
	var created, updated int64
	if err := row.Scan(&t.ID, &t.UserID, &t.Email, &t.Kind, &t.Subject, &t.Status, &t.LastActor, &t.Meta, &created, &updated); err != nil {
		return nil, err
	}
	t.CreatedAt = time.Unix(created, 0)
	t.UpdatedAt = time.Unix(updated, 0)
	return &t, nil
}

// normalizeKind constrains the kind to the ones we understand.
func normalizeKind(k string) string {
	switch k {
	case KindAppeal, KindInvoice:
		return k
	default:
		return KindSupport
	}
}

// clip trims and bounds free text. Subjects and bodies land in the operator
// panel and in email, so an unbounded body is both a storage and a rendering
// problem; truncating is friendlier than rejecting a long complaint outright.
func clip(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) > limit {
		return s[:limit]
	}
	return s
}

// newAccessKey mints the read capability for an anonymous appeal. 32 hex chars
// of crypto/rand — not guessable, and it is the only credential the submitter
// will hold, so it must not be derived from anything enumerable like the ticket
// id or the email.
func newAccessKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Create opens a ticket with its first message. userID may be 0 for an
// anonymous appeal, in which case an access key is minted and returned on the
// ticket; the caller must surface it to the submitter exactly once.
func (s *Service) Create(ctx context.Context, userID int64, email, kind, subject, body string) (*Ticket, error) {
	return s.CreateWithMeta(ctx, userID, email, kind, subject, body, "")
}

// CreateWithMeta is Create plus an opaque JSON payload for the operator UI.
func (s *Service) CreateWithMeta(ctx context.Context, userID int64, email, kind, subject, body, meta string) (*Ticket, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	kind = normalizeKind(kind)
	subject = clip(subject, 200)
	body = clip(body, 8000)
	if email == "" || body == "" {
		return nil, errors.New("email and body are required")
	}
	if subject == "" {
		subject = defaultSubject(kind)
	}
	var key string
	if userID == 0 {
		k, err := newAccessKey()
		if err != nil {
			return nil, err
		}
		key = k
	}
	now := time.Now().Unix()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `INSERT INTO support_tickets
		(user_id, email, kind, subject, status, access_key, last_actor, meta, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, email, kind, subject, StatusOpen, key, AuthorUser, meta, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ticket_messages (ticket_id, author, body, created_at) VALUES (?, ?, ?, ?)`,
		id, AuthorUser, body, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	t, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	t.AccessKey = key
	return t, nil
}

func defaultSubject(kind string) string {
	switch kind {
	case KindAppeal:
		return "账号申诉"
	case KindInvoice:
		return "开票申请"
	default:
		return "咨询"
	}
}

// Get loads one ticket with its full thread.
func (s *Service) Get(ctx context.Context, id int64) (*Ticket, error) {
	t, err := scanTicket(s.DB.QueryRowContext(ctx, `SELECT `+ticketCols+` FROM support_tickets WHERE id = ?`, id))
	if err != nil {
		return nil, ErrNotFound
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, author, body, created_at FROM ticket_messages WHERE ticket_id = ? ORDER BY id ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m Message
		var created int64
		if err := rows.Scan(&m.ID, &m.Author, &m.Body, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(created, 0)
		t.Messages = append(t.Messages, m)
	}
	return t, rows.Err()
}

// GetForKey loads a ticket only if the supplied access key matches — the read
// path for an anonymous appeal. A constant-time comparison is not warranted (the
// key is not a password and each attempt costs a full DB round-trip), but an
// empty key must never match an empty stored key, or every signed-in user's
// ticket would be readable by anyone passing "".
func (s *Service) GetForKey(ctx context.Context, id int64, key string) (*Ticket, error) {
	if strings.TrimSpace(key) == "" {
		return nil, ErrForbidden
	}
	var stored string
	if err := s.DB.QueryRowContext(ctx, `SELECT access_key FROM support_tickets WHERE id = ?`, id).Scan(&stored); err != nil {
		return nil, ErrNotFound
	}
	if stored == "" || stored != key {
		return nil, ErrForbidden
	}
	return s.Get(ctx, id)
}

// ListForUser returns a signed-in user's own tickets, newest first, with the
// total for the shared pagination contract.
func (s *Service) ListForUser(ctx context.Context, userID int64, limit, offset int) ([]*Ticket, int, error) {
	return s.list(ctx, `user_id = ?`, []any{userID}, limit, offset)
}

// ListForAdmin returns the operator queue. status "" means every status; live
// tickets sort first so the queue reads as work-to-do rather than history.
func (s *Service) ListForAdmin(ctx context.Context, status string, limit, offset int) ([]*Ticket, int, error) {
	where, args := `1 = 1`, []any(nil)
	switch status {
	case StatusOpen, StatusPending, StatusResolved, StatusRejected:
		where, args = `status = ?`, []any{status}
	case "live":
		where = `status IN ('open','pending')`
	}
	return s.list(ctx, where, args, limit, offset)
}

func (s *Service) list(ctx context.Context, where string, args []any, limit, offset int) ([]*Ticket, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM support_tickets WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+ticketCols+` FROM support_tickets WHERE `+where+`
		 ORDER BY CASE status WHEN 'open' THEN 0 WHEN 'pending' THEN 1 ELSE 2 END, updated_at DESC
		 LIMIT ? OFFSET ?`, append(append([]any{}, args...), limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*Ticket{}
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// Reply appends a message and moves the ticket's status to reflect who spoke
// last: a user reply reopens the thread, an operator reply parks it awaiting the
// user. A resolved/rejected ticket accepts a user reply (that is how someone
// contests a decision) and reopens.
func (s *Service) Reply(ctx context.Context, ticketID int64, author, body string) error {
	body = clip(body, 8000)
	if body == "" {
		return errors.New("empty message")
	}
	if author != AuthorAdmin {
		author = AuthorUser
	}
	status := StatusOpen
	if author == AuthorAdmin {
		status = StatusPending
	}
	now := time.Now().Unix()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ticket_messages (ticket_id, author, body, created_at) VALUES (?, ?, ?, ?)`,
		ticketID, author, body, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE support_tickets SET status = ?, last_actor = ?, updated_at = ? WHERE id = ?`,
		status, author, now, ticketID); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkSeen records that the ticket's owner has read the thread up to now.
// Called from the user-facing read paths only (userGet, appealGet) — an
// operator opening a ticket must not consume the user's unread marker.
func (s *Service) MarkSeen(ctx context.Context, ticketID int64) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE support_tickets SET user_seen_at = ? WHERE id = ?`, time.Now().Unix(), ticketID)
	return err
}

// UnreadLatest is the newest unread ticket in an unread summary — enough for a
// notification badge to deep-link, no thread content.
type UnreadLatest struct {
	ID        int64  `json:"id"`
	Subject   string `json:"subject"`
	UpdatedAt int64  `json:"updated_at"`
}

// UnreadForUser counts tickets where the operator spoke after the user last
// opened the thread, and returns the most recently updated one (nil when none).
// One query: idx_support_tickets_user narrows to the user's own tickets, and a
// user's ticket count is small enough that the residual filter is free.
func (s *Service) UnreadForUser(ctx context.Context, userID int64) (int, *UnreadLatest, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, subject, updated_at FROM support_tickets
		  WHERE user_id = ? AND last_actor = ? AND updated_at > user_seen_at
		  ORDER BY updated_at DESC`, userID, AuthorAdmin)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var (
		unread int
		latest *UnreadLatest
	)
	for rows.Next() {
		var u UnreadLatest
		if err := rows.Scan(&u.ID, &u.Subject, &u.UpdatedAt); err != nil {
			return 0, nil, err
		}
		if latest == nil {
			latest = &u
		}
		unread++
	}
	return unread, latest, rows.Err()
}

// SetStatus is the operator's explicit close/reopen.
func (s *Service) SetStatus(ctx context.Context, ticketID int64, status string) error {
	switch status {
	case StatusOpen, StatusPending, StatusResolved, StatusRejected:
	default:
		return errors.New("invalid status")
	}
	_, err := s.DB.ExecContext(ctx,
		`UPDATE support_tickets SET status = ?, updated_at = ? WHERE id = ?`, status, time.Now().Unix(), ticketID)
	return err
}

// OpenCount powers the operator panel's queue badge.
func (s *Service) OpenCount(ctx context.Context) int {
	var n int
	_ = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM support_tickets WHERE status = 'open'`).Scan(&n)
	return n
}
