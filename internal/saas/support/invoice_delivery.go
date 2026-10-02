package support

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	stdmail "net/mail"
	"path"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/wjsoj/CPA-Claude/internal/saas/mail"
)

const maxInvoicePDFSize = 8 << 20

func (s *Service) ConfigureInvoiceDelivery(sender mail.AttachmentMailer) {
	s.invoiceMailer = sender
}

// sendInvoice sends an already-issued PDF. Invoice applications continue to
// live in tickets; no new application or issuance state is introduced here.
func (s *Service) sendInvoice(c *gin.Context) {
	if s.invoiceMailer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "发票邮件发送未配置，请检查 Resend 配置"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxInvoicePDFSize+(64<<10))
	err := c.Request.ParseMultipartForm(1 << 20)
	if c.Request.MultipartForm != nil {
		defer c.Request.MultipartForm.RemoveAll()
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "发票 PDF 不能超过 8 MiB"})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请填写邮箱并上传发票 PDF"})
		}
		return
	}
	emails := c.Request.MultipartForm.Value["email"]
	if len(emails) != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请填写一个有效的收件邮箱"})
		return
	}
	email := strings.TrimSpace(emails[0])
	address, err := stdmail.ParseAddress(email)
	if err != nil || address.Address != email || !strings.Contains(email, "@") || strings.ContainsAny(email, "\r\n") || len(email) > 254 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请填写一个有效的收件邮箱"})
		return
	}
	files := c.Request.MultipartForm.File["pdf"]
	if len(files) != 1 || len(c.Request.MultipartForm.File) != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请上传一份发票 PDF"})
		return
	}
	file := files[0]
	if file.Size > maxInvoicePDFSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "发票 PDF 不能超过 8 MiB"})
		return
	}
	filename := path.Base(strings.ReplaceAll(file.Filename, "\\", "/"))
	if !strings.EqualFold(path.Ext(filename), ".pdf") || len(filename) > 255 || strings.ContainsFunc(filename, unicode.IsControl) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请上传有效的 PDF 文件"})
		return
	}
	f, err := file.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无法读取发票文件，请重新上传"})
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxInvoicePDFSize+1))
	if err != nil || !bytes.HasPrefix(data, []byte("%PDF-")) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请上传有效的 PDF 文件"})
		return
	}
	if len(data) > maxInvoicePDFSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "发票 PDF 不能超过 8 MiB"})
		return
	}
	err = s.invoiceMailer.SendAttachments(email, "hypitoken · 您的发票",
		"<p>您好，</p><p>感谢您使用 hypitoken。您的发票已开具，请查收附件 PDF。</p>",
		"您好，\n\n感谢您使用 hypitoken。您的发票已开具，请查收附件 PDF。",
		[]mail.Attachment{{Filename: filename, Content: data, ContentType: "application/pdf"}})
	if err != nil {
		log.Warnf("invoice: Resend delivery failed: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "发票发送失败，请检查 Resend 服务后重试"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"email_sent": true})
}
