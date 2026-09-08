package main

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	loginCSRFCookie = "login_csrf"
	maxFormBodySize = maxDocumentSize + (1 << 20)
)

type loginAttempt struct {
	failures int
	resetAt  time.Time
}

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]loginAttempt
	limit    int
	window   time.Duration
}

func newLoginLimiter(limit int, window time.Duration) *loginLimiter {
	return &loginLimiter{attempts: make(map[string]loginAttempt), limit: limit, window: window}
}

func (limiter *loginLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	attempt, exists := limiter.attempts[key]
	if !exists || !now.Before(attempt.resetAt) {
		delete(limiter.attempts, key)
		return true, 0
	}
	if attempt.failures < limiter.limit {
		return true, 0
	}
	return false, attempt.resetAt.Sub(now)
}

func (limiter *loginLimiter) failure(key string, now time.Time) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	attempt := limiter.attempts[key]
	if !now.Before(attempt.resetAt) {
		attempt = loginAttempt{resetAt: now.Add(limiter.window)}
	}
	attempt.failures++
	limiter.attempts[key] = attempt
}

func (limiter *loginLimiter) success(key string) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	delete(limiter.attempts, key)
}

func loginLimiterKey(remoteAddress, email string) string {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		host = remoteAddress
	}
	return strings.ToLower(strings.TrimSpace(email)) + "\x00" + host
}

func (app *application) csrfDigest(purpose, token string) string {
	return app.sessionDigest(purpose + ":" + token)
}

func (app *application) issueLoginCSRF(c *gin.Context) (string, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(loginCSRFCookie, token, 600, "/login", "", true, true)
	return app.csrfDigest("login", token), nil
}

func (app *application) requireLoginCSRF() gin.HandlerFunc {
	return func(c *gin.Context) {
		cookie, err := c.Cookie(loginCSRFCookie)
		if err != nil || !secureTokenEqual(c.PostForm("_csrf"), app.csrfDigest("login", cookie)) {
			respondMessage(c, http.StatusForbidden, "Форма входа устарела. Обновите страницу и повторите попытку")
			c.Abort()
			return
		}
		c.Next()
	}
}

func (app *application) requireCSRF() gin.HandlerFunc {
	return app.requireCSRFWithLimit(maxFormBodySize)
}

func (app *application) requireCSRFWithLimit(bodyLimit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, bodyLimit)
		sessionToken, err := c.Cookie("session_token")
		if err != nil || !secureTokenEqual(c.PostForm("_csrf"), app.csrfDigest("session", sessionToken)) {
			respondMessage(c, http.StatusForbidden, "Форма устарела. Обновите страницу и повторите действие")
			c.Abort()
			return
		}
		c.Next()
	}
}

func (app *application) templateCSRF(c *gin.Context) string {
	token, err := c.Cookie("session_token")
	if err != nil {
		return ""
	}
	return app.csrfDigest("session", token)
}

func secureTokenEqual(actual, expected string) bool {
	if actual == "" || len(actual) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}
