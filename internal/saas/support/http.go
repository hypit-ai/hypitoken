package support

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	saasauth "github.com/wjsoj/CPA-Claude/internal/saas/auth"
)

// appealKeyHeader carries the appeal access key. It is a bearer credential, so
// it travels in a header rather than the query string: URLs end up in access
// logs, proxy logs, and the Referer sent to any third-party asset the page
// loads, and this key is the ONLY thing standing between a stranger and a
// user's appeal thread.
const appealKeyHeader = "X-Appeal-Key" //nolint:gosec // G101 false positive — a header name, not a credential.

// appealKey reads the caller's access key.
func appealKey(c *gin.Context) string {
	return strings.TrimSpace(c.GetHeader(appealKeyHeader))
}

// PublicRoutes mounts the unauthenticated appeal channel on /api/v2. These are
// the ONLY support endpoints a disabled account can reach — RequireUser rejects
// them everywhere else.
//
// Submission is deliberately open: no session, no emailed code. Requiring one
// would put the appeal channel behind the mail provider, and on the day this
// shipped that provider's daily quota was already exhausted by the very attack
// we were responding to — an appeal path that depends on email is a path that
// fails exactly when it is needed. The trade is that the address on an appeal
// is unverified, so it is a claim for an operator to weigh, not an identity.
// Rate limiting is what keeps the endpoint from being a spam sink, and reads
// still require the access key minted at submission.
func (s *Service) PublicRoutes(g *gin.RouterGroup) {
	a := g.Group("/appeal")
	a.POST("", s.appealSubmit)
	a.GET("/:id", s.appealGet)
	a.POST("/:id/reply", s.appealReply)
}

// UserRoutes mounts the signed-in support desk under /api/v2/me/tickets.
func (s *Service) UserRoutes(g *gin.RouterGroup) {
	t := g.Group("/me/tickets")
	t.GET("", s.userList)
	t.POST("", s.userCreate)
	t.GET("/unread", s.userUnread)
	t.GET("/:id", s.userGet)
	t.POST("/:id/reply", s.userReply)
}

// AdminRoutes mounts the operator queue under /api/v2/admin/tickets.
func (s *Service) AdminRoutes(g *gin.RouterGroup) {
	g.POST("/invoices/send", s.sendInvoice)
	t := g.Group("/tickets")
	t.GET("", s.adminList)
	t.GET("/:id", s.adminGet)
	t.POST("/:id/reply", s.adminReply)
	t.POST("/:id/status", s.adminStatus)
}

// ---- public appeal channel ----

type appealReq struct {
	Email   string `json:"email"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func (s *Service) appealSubmit(c *gin.Context) {
	var req appealReq
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad request"})
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !validEmail(email) || strings.TrimSpace(req.Body) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请填写邮箱与申诉内容"})
		return
	}
	if ok, retry := s.codeRL.allowSubmit(email, c.ClientIP()); !ok {
		c.Header("Retry-After", strconv.Itoa(retry))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "提交过于频繁，请稍后再试", "retry_after": retry})
		return
	}
	// Link the appeal to the account when one exists, so the operator sees the
	// user's history — but an address with no account can still appeal (the
	// account may have been deleted, or they may be writing about a signup that
	// never completed).
	var userID int64
	if u, err := s.DB.GetUserByEmail(c.Request.Context(), email); err == nil {
		userID = u.ID
	}
	// An anonymous appeal keeps user_id = 0 so it is read back by access key.
	// A linked appeal still gets a key: the whole point is that this user cannot
	// log in, so the key is their only way back to the thread.
	t, err := s.Create(c.Request.Context(), 0, email, KindAppeal, req.Subject, req.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if userID > 0 {
		if _, uerr := s.DB.ExecContext(c.Request.Context(),
			`UPDATE support_tickets SET user_id = ? WHERE id = ?`, userID, t.ID); uerr != nil {
			log.Warnf("support: link appeal %d to user %d: %v", t.ID, userID, uerr)
		}
	}
	log.Infof("support: appeal ticket %d filed by %s (user=%d)", t.ID, email, userID)
	c.JSON(http.StatusOK, gin.H{"ticket": t})
}

func (s *Service) appealGet(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	t, err := s.GetForKey(c.Request.Context(), id, appealKey(c))
	if err != nil {
		respondErr(c, err)
		return
	}
	// The key holder IS the ticket's owner reading the thread, so this read
	// consumes the unread marker just like the signed-in path.
	if err := s.MarkSeen(c.Request.Context(), t.ID); err != nil {
		log.Warnf("support: mark ticket %d seen: %v", t.ID, err)
	}
	c.JSON(http.StatusOK, gin.H{"ticket": t})
}

type replyReq struct {
	Body string `json:"body"`
}

func (s *Service) appealReply(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req replyReq
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad request"})
		return
	}
	t, err := s.GetForKey(c.Request.Context(), id, appealKey(c))
	if err != nil {
		respondErr(c, err)
		return
	}
	if ok, retry := s.codeRL.allowSubmit(t.Email, c.ClientIP()); !ok {
		c.Header("Retry-After", strconv.Itoa(retry))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "提交过于频繁，请稍后再试", "retry_after": retry})
		return
	}
	if err := s.Reply(c.Request.Context(), id, AuthorUser, req.Body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	fresh, _ := s.Get(c.Request.Context(), id)
	c.JSON(http.StatusOK, gin.H{"ticket": fresh})
}

// ---- signed-in user desk ----

func (s *Service) userList(c *gin.Context) {
	u := saasauth.CurrentUser(c)
	limit, offset := pageParams(c)
	items, total, err := s.ListForUser(c.Request.Context(), u.ID, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tickets": items, "total": total, "limit": limit, "offset": offset})
}

type createReq struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func (s *Service) userCreate(c *gin.Context) {
	u := saasauth.CurrentUser(c)
	var req createReq
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad request"})
		return
	}
	if ok, retry := s.codeRL.allowSubmit(u.Email, c.ClientIP()); !ok {
		c.Header("Retry-After", strconv.Itoa(retry))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "提交过于频繁，请稍后再试", "retry_after": retry})
		return
	}
	t, err := s.Create(c.Request.Context(), u.ID, u.Email, KindSupport, req.Subject, req.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// A signed-in user reaches their thread through the session; the key would
	// be a second, weaker credential for no benefit.
	t.AccessKey = ""
	c.JSON(http.StatusOK, gin.H{"ticket": t})
}

// ownedTicket loads a ticket and verifies it belongs to the caller.
func (s *Service) ownedTicket(c *gin.Context) (*Ticket, bool) {
	id, ok := pathID(c)
	if !ok {
		return nil, false
	}
	t, err := s.Get(c.Request.Context(), id)
	if err != nil {
		respondErr(c, err)
		return nil, false
	}
	u := saasauth.CurrentUser(c)
	if u == nil || t.UserID != u.ID {
		respondErr(c, ErrForbidden)
		return nil, false
	}
	return t, true
}

func (s *Service) userGet(c *gin.Context) {
	t, ok := s.ownedTicket(c)
	if !ok {
		return
	}
	// User path only — adminGet must not consume the user's unread marker.
	if err := s.MarkSeen(c.Request.Context(), t.ID); err != nil {
		log.Warnf("support: mark ticket %d seen: %v", t.ID, err)
	}
	c.JSON(http.StatusOK, gin.H{"ticket": t})
}

// userUnread powers the in-app notification badge: with no outbound mail, this
// endpoint is the only way a user learns an operator has replied.
func (s *Service) userUnread(c *gin.Context) {
	u := saasauth.CurrentUser(c)
	unread, latest, err := s.UnreadForUser(c.Request.Context(), u.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Explicit null keeps the contract shape stable when there is nothing unread.
	var latestJSON any
	if latest != nil {
		latestJSON = latest
	}
	c.JSON(http.StatusOK, gin.H{"unread": unread, "latest": latestJSON})
}

func (s *Service) userReply(c *gin.Context) {
	t, ok := s.ownedTicket(c)
	if !ok {
		return
	}
	var req replyReq
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad request"})
		return
	}
	if err := s.Reply(c.Request.Context(), t.ID, AuthorUser, req.Body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	fresh, _ := s.Get(c.Request.Context(), t.ID)
	c.JSON(http.StatusOK, gin.H{"ticket": fresh})
}

// ---- operator queue ----

func (s *Service) adminList(c *gin.Context) {
	limit, offset := pageParams(c)
	items, total, err := s.ListForAdmin(c.Request.Context(), c.Query("status"), limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"tickets": items, "total": total, "limit": limit, "offset": offset,
		"open": s.OpenCount(c.Request.Context()),
	})
}

func (s *Service) adminGet(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	t, err := s.Get(c.Request.Context(), id)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ticket": t})
}

func (s *Service) adminReply(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req replyReq
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad request"})
		return
	}
	if _, err := s.Get(c.Request.Context(), id); err != nil {
		respondErr(c, err)
		return
	}
	if err := s.Reply(c.Request.Context(), id, AuthorAdmin, req.Body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	fresh, _ := s.Get(c.Request.Context(), id)
	c.JSON(http.StatusOK, gin.H{"ticket": fresh})
}

type statusReq struct {
	Status string `json:"status"`
}

func (s *Service) adminStatus(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req statusReq
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad request"})
		return
	}
	if err := s.SetStatus(c.Request.Context(), id, req.Status); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	fresh, _ := s.Get(c.Request.Context(), id)
	c.JSON(http.StatusOK, gin.H{"ticket": fresh})
}

// ---- helpers ----

func pathID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return 0, false
	}
	return id, true
}

func pageParams(c *gin.Context) (limit, offset int) {
	limit, _ = strconv.Atoi(c.Query("limit"))
	offset, _ = strconv.Atoi(c.Query("offset"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func respondErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "工单不存在"})
	case errors.Is(err, ErrForbidden):
		// Same body as not-found so an access key cannot be probed for validity.
		c.JSON(http.StatusNotFound, gin.H{"error": "工单不存在"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// validEmail is the same shallow check the auth package applies: exactly one
// "@", something either side, no spaces. Real validation is the OTP.
func validEmail(s string) bool {
	if len(s) < 3 || len(s) > 254 || strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	at := strings.IndexByte(s, '@')
	return at > 0 && at < len(s)-1 && strings.IndexByte(s[at+1:], '@') < 0 && strings.Contains(s[at+1:], ".")
}
