package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestMailConfiguration(t *testing.T) {
	for _, name := range []string{"SMTP_HOST", "SMTP_PORT", "SMTP_SECURITY", "SMTP_USERNAME", "SMTP_PASSWORD", "SMTP_FROM", "APP_BASE_URL"} {
		t.Setenv(name, "")
	}
	cfg, err := loadMailConfig()
	if err != nil || cfg != nil {
		t.Fatal("SMTP must be optional")
	}
	t.Setenv("SMTP_HOST", "smtp.timeweb.ru")
	t.Setenv("SMTP_USERNAME", "sender@example.com")
	t.Setenv("SMTP_FROM", "sender@example.com")
	t.Setenv("SMTP_PASSWORD", "test-only")
	t.Setenv("APP_BASE_URL", "https://approvals.example.com")
	cfg, err = loadMailConfig()
	if err != nil || cfg.Port != "465" || cfg.Mode != "tls" {
		t.Fatalf("default TLS config: %v", err)
	}
	for _, tt := range []struct{ key, value string }{{"SMTP_SECURITY", "none"}, {"SMTP_PORT", "0"}, {"SMTP_PORT", "65536"}, {"SMTP_FROM", "sender@example.com\r\nBcc: other@example.com"}, {"APP_BASE_URL", "http://example.com"}, {"APP_BASE_URL", "https://user:pass@example.com"}, {"APP_BASE_URL", "https://example.com/path"}, {"APP_BASE_URL", "https://example.com?token=x"}} {
		t.Run(tt.key+tt.value, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			if _, err := loadMailConfig(); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
	t.Setenv("SMTP_PORT", "587")
	t.Setenv("SMTP_SECURITY", "starttls")
	if _, err := loadMailConfig(); err != nil {
		t.Fatal(err)
	}
}
func TestEncryptedEmailAndTampering(t *testing.T) {
	app := &application{sessionSecret: []byte("01234567890123456789012345678901")}
	body := "Одноразовая ссылка #test-token"
	a, err := app.encryptMail(body)
	if err != nil {
		t.Fatal(err)
	}
	b, err := app.encryptMail(body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) || bytes.Contains(a, []byte("test-token")) {
		t.Fatal("encryption must use a fresh nonce and hide the token")
	}
	got, err := app.decryptMail(a)
	if err != nil || got != body {
		t.Fatal("cannot decrypt")
	}
	a[len(a)-1] ^= 1
	if _, err = app.decryptMail(a); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	if _, err = app.decryptMail(nil); err == nil {
		t.Fatal("empty ciphertext accepted")
	}
	app.sessionSecret = []byte("another-secret")
	if _, err = app.decryptMail(b); err == nil {
		t.Fatal("wrong key accepted")
	}
}
func TestSMTPMessageEncoding(t *testing.T) {
	subject := "Начато согласование"
	body := strings.Repeat("Проверьте документ.\n", 20)
	data, err := smtpMessage("sender@example.com", "user@example.com", subject, body, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	m, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	got, err := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil || got != subject {
		t.Fatal("subject encoding")
	}
	decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, m.Body))
	if err != nil || string(decoded) != body {
		t.Fatal("body encoding")
	}
	for _, line := range strings.Split(strings.SplitN(string(data), "\r\n\r\n", 2)[1], "\r\n") {
		if len(line) > 76 {
			t.Fatal("base64 line too long")
		}
	}
	if _, err = smtpMessage("sender@example.com", "user@example.com", "hello\r\nBcc: leak@example.com", body, "test@example.com"); err == nil {
		t.Fatal("header injection")
	}
}
func TestEmailCSRFAndRoleProtection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := &application{mail: &mailConfig{}, sessionSecret: []byte("01234567890123456789012345678901")}
	for _, valid := range []bool{true, false} {
		router := gin.New()
		router.POST("/password/reset", app.requireEmailCSRF(), func(c *gin.Context) { c.Status(204) })
		csrf := app.csrfDigest("email", "cookie")
		if !valid {
			csrf = "wrong"
		}
		req := httptest.NewRequest("POST", "/password/reset", strings.NewReader(url.Values{"_csrf": {csrf}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: "email_csrf", Value: "cookie"})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		want := 204
		if !valid {
			want = 403
		}
		if w.Code != want {
			t.Fatalf("CSRF status %d", w.Code)
		}
	}
	for _, role := range []string{"admin", "secretary", "committee", "approver", "observer"} {
		r := gin.New()
		r.POST("/admin/users", func(c *gin.Context) { c.Set("user", user{Role: role}) }, app.requireRole("admin"), func(c *gin.Context) { c.Status(204) })
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", "/admin/users", nil))
		want := 403
		if role == "admin" {
			want = 204
		}
		if w.Code != want {
			t.Fatalf("role %s status %d", role, w.Code)
		}
	}
}

type emailCaptureExecutor struct {
	calls int
	sql   string
	args  []any
}

func (e *emailCaptureExecutor) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	e.calls++
	e.sql = sql
	e.args = args
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}
func TestWorkflowMailSelection(t *testing.T) {
	app := &application{mail: &mailConfig{BaseURL: "https://example.com"}, sessionSecret: []byte("test-secret")}
	id := int64(10)
	for _, event := range []string{"internal_review.started", "internal_review.restarted", "internal_review.visa_submitted", "internal_review.completed", "committee_vote.started", "committee_vote.submitted", "committee_vote.extended", "committee_vote.cancelled", "committee_vote.completed", "question.cancelled", "question.file_version_uploaded", "question.file_version_confirmed", "question.file_version_rejected", "question.decision_revised"} {
		e := &emailCaptureExecutor{}
		err := app.queueWorkflowMail(context.Background(), e, user{ID: 2}, auditRecord{EventType: event, QuestionID: &id, Details: "private comments"})
		if err != nil || e.calls != 1 {
			t.Fatalf("event %s: %v", event, err)
		}
		body, err := app.decryptMail(e.args[1].([]byte))
		if err != nil || !strings.Contains(body, "https://example.com/questions/10") || strings.Contains(body, "private comments") {
			t.Fatal("unsafe mail content")
		}
	}
	e := &emailCaptureExecutor{}
	if err := app.queueWorkflowMail(context.Background(), e, user{}, auditRecord{EventType: "session.login", QuestionID: &id}); err != nil || e.calls != 0 {
		t.Fatal("unexpected login notification")
	}
	app.mail = nil
	if err := app.queueWorkflowMail(context.Background(), e, user{}, auditRecord{EventType: "committee_vote.started", QuestionID: &id}); err != nil || e.calls != 0 {
		t.Fatal("SMTP disabled")
	}
}
func TestForgotPasswordRateLimitBeforeDatabase(t *testing.T) {
	app := &application{mail: &mailConfig{}, loginLimiter: newLoginLimiter(1, time.Minute), sessionSecret: []byte("test-secret")}
	key := "email-reset:" + loginLimiterKey("192.0.2.1:1234", "")
	app.loginLimiter.failure(key, time.Now())
	r := gin.New()
	r.LoadHTMLGlob("templates/*")
	r.POST("/password/forgot", app.forgotPassword)
	req := httptest.NewRequest("POST", "/password/forgot", strings.NewReader("email=user%40example.com"))
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 429 {
		t.Fatalf("got %d", w.Code)
	}
}
