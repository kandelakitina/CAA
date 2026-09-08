//go:build localintegration

package main

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// Opt-in, read-only fixture server for browser QA; no database or real accounts.
func TestLocalUIPreview(t *testing.T) {
	if os.Getenv("NEVA_UI_PREVIEW") != "1" {
		t.Skip("opt-in visual fixture")
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.LoadHTMLGlob("templates/*")
	r.Static("/static", "./static")
	actor := user{FullName: "Администратор", Role: "admin"}
	r.GET("/admin", func(c *gin.Context) {
		c.HTML(200, "admin.html", gin.H{"Title": "Панель администратора", "User": actor, "Summary": adminSnapshot{Questions: 12, Documents: 3, Users: 9, ActiveRounds: 4, Files: 28}, "Materials": []adminMaterial{{ID: 12, Kind: "question", Title: "Бюджет концертного сезона", Status: "Внутреннее согласование"}, {ID: 11, Kind: "question", Title: "Договор на поставку оборудования", Status: "Голосование Комитета"}, {ID: 10, Kind: "document", Title: "Положение о закупках", Status: "Одобрен", Archived: true}}})
	})
	r.GET("/reports/statuses", func(c *gin.Context) {
		c.HTML(200, "status-report.html", gin.H{"Title": "Статусы согласования", "User": actor, "Rows": []statusReportRow{{ID: 12, Title: "Бюджет концертного сезона", Status: "Внутреннее согласование", Internal: "Идёт согласование", InternalDone: 3, InternalTotal: 4, InternalDeadline: "15.09.2026", InternalPending: "Юридическая дирекция", Committee: "Не запускалось"}, {ID: 11, Title: "Договор на поставку оборудования", Status: "Голосование Комитета", Internal: "Готов к передаче", InternalDone: 8, InternalTotal: 8, Committee: "Идёт согласование", CommitteeDone: 2, CommitteeTotal: 5, CommitteeDeadline: "16.09.2026", CommitteePending: "Иван Петров, Мария Смирнова"}}, "ExportURL": "/reports/statuses.csv"})
	})
	server := &http.Server{Addr: "127.0.0.1:18089", Handler: r, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			t.Error(err)
		}
	}()
	t.Log("Read-only visual preview: http://127.0.0.1:18089/admin")
	defer server.Shutdown(context.Background())
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(".local-test/stop-ui-preview"); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}
