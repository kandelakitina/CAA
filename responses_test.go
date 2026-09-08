package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestBrowserErrorPresentation(t *testing.T) {
	app := &application{}
	for _, role := range []string{"admin", "secretary", "observer", ""} {
		t.Run(role, func(t *testing.T) {
			router := app.routes()
			router.POST("/questions/:id/test-error", func(c *gin.Context) {
				if role != "" {
					c.Set("user", user{Role: role, FullName: "Тест"})
				}
				respondMessage(c, 422, "Нет активного представителя: %s", "<script>Строительная дирекция</script>")
			})
			request := httptest.NewRequest("POST", "/questions/2/test-error", nil)
			request.Header.Set("Accept", "text/html,application/xhtml+xml")
			result := httptest.NewRecorder()
			router.ServeHTTP(result, request)
			body := result.Body.String()
			if result.Code != 422 || !strings.HasPrefix(result.Header().Get("Content-Type"), "text/html") || !strings.Contains(body, "&lt;script&gt;Строительная дирекция&lt;/script&gt;") {
				t.Fatal("browser error must preserve status and escape the message")
			}
			if strings.Contains(body, `href="/admin/users"`) != (role == "admin") {
				t.Fatal("administrator navigation visibility is incorrect")
			}
			if role != "" && (!strings.Contains(body, "Neva Concert Hall") || !strings.Contains(body, `href="/questions/2"`)) {
				t.Fatal("application shell or safe question link missing")
			}
			if role == "" && !strings.Contains(body, `href="/login"`) {
				t.Fatal("anonymous error must link to login")
			}
			request.Header.Set("Accept", "text/plain")
			plain := httptest.NewRecorder()
			router.ServeHTTP(plain, request)
			if plain.Code != 422 || !strings.HasPrefix(plain.Header().Get("Content-Type"), "text/plain") {
				t.Fatal("non-HTML clients must retain plain errors")
			}
		})
	}
}
