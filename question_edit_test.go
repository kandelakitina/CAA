package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type questionEditTx struct {
	pgx.Tx
	status                                       string
	hasHistory, failAudit, committed, rolledBack bool
	updatedAt                                    time.Time
	updateArgs, auditArgs                        []any
}

func (tx *questionEditTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	return cancellationRow(func(dest ...any) error {
		if strings.Contains(sql, "SELECT EXISTS") {
			*dest[0].(*bool) = tx.hasHistory
			return nil
		}
		if !strings.HasSuffix(sql, "FOR UPDATE") {
			return errors.New("edit must lock the question")
		}
		if tx.status == "missing" {
			return pgx.ErrNoRows
		}
		values := []string{"budget", "", "Старое название", "", "Старая формулировка"}
		for i, value := range values {
			*dest[i].(*string) = value
		}
		*dest[5].(*time.Time) = time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
		for i := 6; i <= 8; i++ {
			*dest[i].(*string) = ""
		}
		*dest[9].(*string), *dest[10].(*time.Time) = tx.status, tx.updatedAt
		return nil
	})
}

func (tx *questionEditTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "UPDATE questions") {
		tx.updateArgs = args
	} else if strings.Contains(sql, "INSERT INTO audit_events") {
		if tx.failAudit {
			return pgconn.CommandTag{}, errors.New("audit unavailable")
		}
		tx.auditArgs = args
	} else {
		return pgconn.CommandTag{}, errors.New("unexpected write")
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (tx *questionEditTx) Commit(context.Context) error   { tx.committed = true; return nil }
func (tx *questionEditTx) Rollback(context.Context) error { tx.rolledBack = true; return nil }

func TestQuestionEditTransaction(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 123000, time.UTC)
	for _, test := range []struct {
		name, status              string
		history, stale, failAudit bool
		wantErr                   error
	}{
		{name: "fresh draft", status: "draft"},
		{name: "cancelled round returned to draft", status: "draft", history: true, wantErr: errQuestionCannotEdit},
		{name: "stale tab", status: "draft", stale: true, wantErr: errQuestionEditConflict},
		{name: "audit failure", status: "draft", failAudit: true},
		{name: "not found", status: "missing", wantErr: pgx.ErrNoRows},
		{name: "internal review", status: "internal_review", wantErr: errQuestionCannotEdit},
		{name: "revision", status: "revision_required", wantErr: errQuestionCannotEdit},
		{name: "ready", status: "ready_for_committee", wantErr: errQuestionCannotEdit},
		{name: "committee", status: "committee_voting", wantErr: errQuestionCannotEdit},
		{name: "approved", status: "approved", wantErr: errQuestionCannotEdit},
		{name: "rejected", status: "rejected", wantErr: errQuestionCannotEdit},
		{name: "no quorum", status: "no_quorum", wantErr: errQuestionCannotEdit},
		{name: "cancelled", status: "cancelled", wantErr: errQuestionCannotEdit},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx := &questionEditTx{status: test.status, hasHistory: test.history, failAudit: test.failAudit, updatedAt: now}
			expected := now
			if test.stale {
				expected = now.Add(-time.Second)
			}
			input := questionInput{QuestionType: "transaction", TransactionType: "financial", Title: "Заём", DecisionText: "Одобрить заём", Counterparty: "Компания", Amount: "12000.50", Currency: "RUB"}
			err := (&application{}).updateQuestionTransaction(context.Background(), tx, user{ID: 7, Role: "secretary"}, 1, input, now, expected)
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("err=%v want=%v", err, test.wantErr)
			}
			allowed := test.wantErr == nil && !test.failAudit
			if (err == nil) != allowed || tx.committed != allowed || !tx.rolledBack {
				t.Fatalf("err=%v commit=%v rollback=%v", err, tx.committed, tx.rolledBack)
			}
			if test.wantErr != nil && len(tx.updateArgs) != 0 {
				t.Fatal("forbidden/stale edit wrote data")
			}
			if allowed {
				if tx.updateArgs[1] != "transaction" || tx.updateArgs[5] != input.DecisionText || tx.updateArgs[8] != "12000.50" {
					t.Fatalf("wrong update args: %v", tx.updateArgs)
				}
				if tx.auditArgs[4] != "question.updated" {
					t.Fatal("missing edit audit")
				}
				details := tx.auditArgs[11].(string)
				if !strings.Contains(details, "Старая формулировка") || !strings.Contains(details, "Одобрить заём") {
					t.Fatal("audit must preserve old and new text")
				}
			}
		})
	}
}

func TestQuestionEditHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := &application{sessionSecret: []byte("edit-test-secret")}
	for _, role := range []string{"secretary", "admin", "committee", "approver", "observer"} {
		for _, csrf := range []bool{true, false} {
			router := gin.New()
			router.LoadHTMLGlob("templates/*")
			setUser := func(c *gin.Context) { c.Set("user", user{Role: role}) }
			router.GET("/questions/:id/edit", setUser, app.requireRole("secretary"), app.showQuestionEdit)
			router.POST("/questions/:id/edit", setUser, app.requireCSRF(), app.requireRole("secretary"), app.updateQuestion)
			get := httptest.NewRecorder()
			router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/questions/invalid/edit", nil))
			wantGet := http.StatusForbidden
			if role == "secretary" {
				wantGet = http.StatusBadRequest
			}
			if get.Code != wantGet {
				t.Fatalf("GET role=%s status=%d", role, get.Code)
			}
			form := url.Values{"title": {"Сохранить мой ввод"}, "decision_text": {"<script>alert(1)</script>"}, "edit_token": {time.Now().Format(time.RFC3339Nano)}}
			if csrf {
				form.Set("_csrf", app.csrfDigest("session", "edit-session"))
			}
			request := httptest.NewRequest(http.MethodPost, "/questions/1/edit", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.AddCookie(&http.Cookie{Name: "session_token", Value: "edit-session"})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			want := http.StatusForbidden
			if role == "secretary" && csrf {
				want = http.StatusUnprocessableEntity
			}
			if response.Code != want {
				t.Fatalf("role=%s csrf=%v status=%d want=%d", role, csrf, response.Code, want)
			}
			if want == http.StatusUnprocessableEntity {
				body := response.Body.String()
				if !strings.Contains(body, "Сохранить мой ввод") || !strings.Contains(body, "&lt;script&gt;") || strings.Contains(body, "<script>") {
					t.Fatal("validation must preserve and escape input")
				}
			}
		}
	}
}
