package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestLoginLimiter(t *testing.T) {
	limiter := newLoginLimiter(2, time.Minute)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	key := loginLimiterKey("192.0.2.10:1234", "User@Example.com")

	if allowed, _ := limiter.allow(key, now); !allowed {
		t.Fatal("new login key must be allowed")
	}
	limiter.failure(key, now)
	limiter.failure(key, now)
	if allowed, retry := limiter.allow(key, now); allowed || retry != time.Minute {
		t.Fatalf("blocked key: allowed=%v retry=%v", allowed, retry)
	}
	if allowed, _ := limiter.allow(key, now.Add(time.Minute)); !allowed {
		t.Fatal("key must be allowed after the window")
	}

	limiter.failure(key, now.Add(2*time.Minute))
	limiter.success(key)
	if allowed, _ := limiter.allow(key, now.Add(2*time.Minute)); !allowed {
		t.Fatal("successful login must clear failures")
	}
}

func TestRequireCSRF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := &application{sessionSecret: []byte("01234567890123456789012345678901")}
	sessionToken := "session-token"
	validToken := app.csrfDigest("session", sessionToken)

	tests := []struct {
		name       string
		formToken  string
		cookie     bool
		wantStatus int
	}{
		{name: "valid", formToken: validToken, cookie: true, wantStatus: http.StatusNoContent},
		{name: "missing form token", cookie: true, wantStatus: http.StatusForbidden},
		{name: "wrong form token", formToken: "wrong", cookie: true, wantStatus: http.StatusForbidden},
		{name: "missing cookie", formToken: validToken, wantStatus: http.StatusForbidden},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			router.POST("/action", app.requireCSRF(), func(c *gin.Context) {
				c.Status(http.StatusNoContent)
			})
			form := url.Values{"_csrf": []string{test.formToken}}
			request := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if test.cookie {
				request.AddCookie(&http.Cookie{Name: "session_token", Value: sessionToken})
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}

func TestRequireLoginCSRF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := &application{sessionSecret: []byte("01234567890123456789012345678901")}
	cookieToken := "login-cookie-token"
	form := url.Values{"_csrf": []string{app.csrfDigest("login", cookieToken)}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: loginCSRFCookie, Value: cookieToken})

	router := gin.New()
	router.POST("/login", app.requireLoginCSRF(), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestRequireRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		role       string
		wantStatus int
	}{
		{role: "admin", wantStatus: http.StatusNoContent},
		{role: "observer", wantStatus: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.role, func(t *testing.T) {
			app := &application{}
			router := gin.New()
			router.GET("/admin", func(c *gin.Context) {
				c.Set("user", user{Role: test.role})
			}, app.requireRole("admin"), func(c *gin.Context) {
				c.Status(http.StatusNoContent)
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin", nil))
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}

func TestEveryPostFormContainsCSRFToken(t *testing.T) {
	formPattern := regexp.MustCompile(`(?s)<form\b[^>]*method="post"[^>]*>.*?</form>`)
	files, err := filepath.Glob("templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, filename := range files {
		contents, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		for _, form := range formPattern.FindAll(contents, -1) {
			if !strings.Contains(string(form), `name="_csrf"`) {
				t.Errorf("POST form without CSRF token in %s", filename)
			}
		}
	}
}
