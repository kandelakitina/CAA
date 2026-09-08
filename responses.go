package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Browser errors share the application shell; non-HTML clients retain plain text.
func respondMessage(c *gin.Context, status int, format string, values ...any) {
	configured, ok := c.Get("application")
	if !ok || status < 400 || !strings.Contains(c.GetHeader("Accept"), "text/html") || c.GetHeader("HX-Request") == "true" {
		c.String(status, format, values...)
		return
	}
	app := configured.(*application)
	actor, signedIn := c.Get("user")
	backURL, backLabel := "/login", "К странице входа"
	if signedIn {
		backURL, backLabel = "/registry", "К списку вопросов"
		if id, err := strconv.ParseInt(c.Param("id"), 10, 64); err == nil && id > 0 {
			if strings.HasPrefix(c.FullPath(), "/questions/:id") {
				backURL, backLabel = fmt.Sprintf("/questions/%d", id), "Вернуться к вопросу"
			} else if strings.HasPrefix(c.FullPath(), "/documents/:id") {
				backURL, backLabel = fmt.Sprintf("/documents/%d", id), "Вернуться к документу"
			}
		}
	}
	title := "Не удалось выполнить действие"
	if status == http.StatusForbidden {
		title = "Действие недоступно"
	}
	if status == http.StatusNotFound {
		title = "Не удалось найти запись"
	}
	c.Header("Cache-Control", "no-store")
	c.HTML(status, "error.html", gin.H{
		"Title": title, "Message": fmt.Sprintf(format, values...),
		"User": actor, "SignedIn": signedIn, "CSRFToken": app.templateCSRF(c),
		"BackURL": backURL, "BackLabel": backLabel,
	})
}
