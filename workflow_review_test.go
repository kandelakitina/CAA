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
)

// The latest completed round was positive; an older failed round must not be
// used as the source after Committee rework.
type repeatReviewTestTx struct {
	decisionTestTx
	sourceRound int64
}

func (tx *repeatReviewTestTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "SELECT title, status, updated_at"):
		updated := decisionTestTime
		if tx.stale {
			updated = updated.Add(time.Second)
		}
		return decisionTestRow{"Вопрос", tx.status, updated}
	case strings.Contains(sql, "SELECT id FROM internal_review_rounds"):
		if strings.Contains(sql, "outcome =") || !strings.Contains(sql, "ORDER BY completed_at DESC, id DESC LIMIT 1") {
			return cancellationRow(func(...any) error { return errors.New("must use latest completed round regardless of outcome") })
		}
		return decisionTestRow{int64(10)}
	case strings.Contains(sql, "INSERT INTO internal_review_rounds"):
		tx.writes++
		if !strings.Contains(sql, "decision_text FROM questions") {
			return cancellationRow(func(...any) error { return errors.New("missing decision snapshot") })
		}
		return decisionTestRow{int64(11)}
	default:
		return tx.decisionTestTx.QueryRow(ctx, sql, args...)
	}
}

func (tx *repeatReviewTestTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "COALESCE(v.source_visa_id") {
		tx.sourceRound = args[0].(int64)
	}
	return tx.decisionTestTx.Query(ctx, sql, args...)
}

func TestRevisionPlanAfterCommitteeRework(t *testing.T) {
	for _, role := range []string{"secretary", "observer"} {
		tx := &repeatReviewTestTx{decisionTestTx: decisionTestTx{changedFile: true}}
		plan, err := loadRevisionPlan(context.Background(), tx, 1, "revision_required", user{Role: role})
		if err != nil || !plan.Ready || tx.sourceRound != 10 || len(plan.Files) != 1 {
			t.Fatalf("plan=%+v source=%d err=%v", plan, tx.sourceRound, err)
		}
		if plan.CanRestart != (role == "secretary" || role == "admin") {
			t.Fatal("only the secretary may restart")
		}
		for _, service := range plan.Files[0].Services {
			if !service.Selectable || !service.Carried {
				t.Fatal("positive source visas must offer an explicit repeat/carry choice")
			}
		}
	}
}

func TestRepeatReviewTransaction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, name := range []string{"carry all", "request finance", "stale form", "pending upload", "unchanged rejected", "wrong state", "audit failure"} {
		t.Run(name, func(t *testing.T) {
			tx := &repeatReviewTestTx{decisionTestTx: decisionTestTx{status: "revision_required", changedFile: true}}
			selected := map[string]bool{}
			want := http.StatusSeeOther
			switch name {
			case "request finance":
				selected[revisionVisaKey(5, "finance")] = true
			case "stale form":
				tx.stale, want = true, http.StatusConflict
			case "pending upload":
				tx.pendingUploads, want = 1, http.StatusConflict
			case "unchanged rejected":
				tx.rejected, tx.changedFile, want = true, false, http.StatusUnprocessableEntity
			case "wrong state":
				tx.status, want = "approved", http.StatusConflict
			case "audit failure":
				tx.failAudit, want = true, http.StatusInternalServerError
			}
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			form := url.Values{"question_context": {decisionTestTime.Format(time.RFC3339Nano)}}
			c.Request = httptest.NewRequest(http.MethodPost, "/questions/1/internal-review/restart", strings.NewReader(form.Encode()))
			c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			app := &application{}
			func() {
				defer tx.Rollback(c.Request.Context())
				app.restartInternalReviewInTx(c, tx, user{ID: 2, Role: "secretary"}, 1, decisionTestTime.AddDate(0, 0, 7), selected)
			}()
			c.Writer.WriteHeaderNow()
			if response.Code != want || tx.committed != (want == http.StatusSeeOther) || !tx.rolledBack {
				t.Fatalf("status=%d body=%s committed=%v rollback=%v", response.Code, response.Body.String(), tx.committed, tx.rolledBack)
			}
			if name == "carry all" && (!tx.completed || tx.carried != 4 || tx.pendingRequirements != 0) {
				t.Fatal("all carried visas must complete the round immediately")
			}
			if name == "request finance" && (tx.completed || tx.carried != 3 || tx.pendingRequirements != 1) {
				t.Fatal("a requested fresh visa must keep the round open")
			}
		})
	}
}

func TestQuestionUploadTransitions(t *testing.T) {
	for _, status := range []string{"draft", "internal_review", "revision_required", "ready_for_committee", "rejected", "no_quorum", "committee_voting", "approved", "cancelled", "unknown"} {
		next, allowed := questionStatusAfterFileUpload(status)
		wantAllowed := status == "draft" || status == "internal_review" || status == "revision_required" || status == "ready_for_committee" || status == "rejected" || status == "no_quorum"
		if allowed != wantAllowed {
			t.Fatalf("status=%s allowed=%v", status, allowed)
		}
		if status == "ready_for_committee" || status == "rejected" || status == "no_quorum" {
			if next != "revision_required" {
				t.Fatalf("changed reviewed bundle must return to review: %s -> %s", status, next)
			}
		} else if next != status {
			t.Fatalf("unexpected transition: %s -> %s", status, next)
		}
	}
}

func TestRepeatReviewRolesAndCSRF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := &application{sessionSecret: []byte("repeat-review-test")}
	for _, role := range []string{"secretary", "admin", "approver", "committee", "observer"} {
		for _, csrf := range []bool{true, false} {
			router := gin.New()
			router.POST("/questions/:id/internal-review/restart", func(c *gin.Context) { c.Set("user", user{Role: role}) }, app.requireCSRF(), app.requireRole("admin", "secretary"), app.restartInternalReview)
			form := url.Values{}
			if csrf {
				form.Set("_csrf", app.csrfDigest("session", "test-session"))
			}
			request := httptest.NewRequest(http.MethodPost, "/questions/1/internal-review/restart", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.AddCookie(&http.Cookie{Name: "session_token", Value: "test-session"})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			want := http.StatusForbidden
			if (role == "secretary" || role == "admin") && csrf {
				want = http.StatusUnprocessableEntity // Allowed through to deadline validation.
			}
			if response.Code != want {
				t.Fatalf("role=%s csrf=%v status=%d", role, csrf, response.Code)
			}
		}
	}
}
