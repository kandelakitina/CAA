package main

import (
	"html/template"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestParseAuditFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name    string
		query   string
		wantErr bool
	}{
		{name: "empty", query: ""},
		{name: "valid", query: "event_type=question.created&actor_id=2&date_from=2026-01-01&date_to=2026-01-31"},
		{name: "bad actor", query: "actor_id=zero", wantErr: true},
		{name: "bad date", query: "date_from=31.01.2026", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			context, _ := gin.CreateTestContext(httptest.NewRecorder())
			context.Request = httptest.NewRequest("GET", "/admin/audit?"+test.query, nil)
			_, err := parseAuditFilters(context)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestAuditEventLabels(t *testing.T) {
	if got := auditEventLabel("question.created"); got != "Создан вопрос" {
		t.Fatalf("unexpected label: %q", got)
	}
	if got := auditEventLabel("future.event"); got != "Событие: future.event" {
		t.Fatalf("unexpected fallback label: %q", got)
	}
}

func TestTemplatesParse(t *testing.T) {
	if _, err := template.ParseGlob("templates/*.html"); err != nil {
		t.Fatal(err)
	}
}
