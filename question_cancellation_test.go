package main

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCancelledQuestionTemplate(t *testing.T) {
	tmpl, err := template.ParseFiles("templates/question.html")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = tmpl.Execute(&output, gin.H{
		"User":           user{Role: "secretary"},
		"Question":       questionDetail{ID: 1, CancellationReason: "<script>alert(1)</script>", CancelledBy: "Секретарь"},
		"InternalReview": internalReviewView{}, "CommitteeVote": committeeVoteView{}, "RevisionPlan": revisionPlanView{},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := output.String()
	if strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("cancellation reason must be escaped")
	}
	if strings.Contains(body, `action="/questions/1/cancel"`) {
		t.Fatal("cancelled question must not offer cancellation again")
	}
}

func TestQuestionCancellationReason(t *testing.T) {
	for _, reason := range []string{"", " \n\t", strings.Repeat("я", 2001)} {
		if _, err := validateQuestionCancellationReason(reason); err == nil {
			t.Fatalf("accepted invalid reason of length %d", len([]rune(reason)))
		}
	}
	for _, reason := range []string{"Причина", strings.Repeat("я", 2000)} {
		got, err := validateQuestionCancellationReason(" \n" + reason + " ")
		if err != nil || got != reason {
			t.Fatalf("reason validation: %q, %v", got, err)
		}
	}
}

type cancellationRow func(...any) error

func (r cancellationRow) Scan(dest ...any) error { return r(dest...) }

type cancellationTx struct {
	pgx.Tx
	status                string
	failAudit             bool
	committed, rolledBack bool
	events                []string
	updated               bool
}

func (tx *cancellationTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	return cancellationRow(func(dest ...any) error {
		if strings.Contains(sql, "SELECT title, status") {
			if tx.status == "missing" {
				return pgx.ErrNoRows
			}
			*dest[0].(*string), *dest[1].(*string) = "Вопрос", tx.status
			return nil
		}
		if (tx.status == "internal_review" && strings.Contains(sql, "internal_review_rounds")) ||
			(tx.status == "committee_voting" && strings.Contains(sql, "committee_vote_rounds")) {
			*dest[0].(*int64) = 42
			return nil
		}
		return pgx.ErrNoRows
	})
}

func (tx *cancellationTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "INSERT INTO audit_events") {
		if tx.failAudit {
			return pgconn.CommandTag{}, errors.New("audit unavailable")
		}
		tx.events = append(tx.events, args[4].(string))
	} else if strings.Contains(sql, "UPDATE questions") {
		tx.updated = true
	} else {
		return pgconn.CommandTag{}, errors.New("unexpected write")
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (tx *cancellationTx) Commit(context.Context) error   { tx.committed = true; return nil }
func (tx *cancellationTx) Rollback(context.Context) error { tx.rolledBack = true; return nil }

func TestQuestionCancellationTransaction(t *testing.T) {
	for _, status := range []string{"draft", "internal_review", "revision_required", "ready_for_committee", "committee_voting", "rejected", "no_quorum", "approved", "cancelled", "unknown", "missing"} {
		t.Run(status, func(t *testing.T) {
			tx := &cancellationTx{status: status}
			err := (&application{}).cancelQuestionTransaction(context.Background(), tx, user{Role: "secretary"}, 1, "Причина")
			allowed := status != "approved" && status != "cancelled" && status != "unknown" && status != "missing"
			if (err == nil) != allowed || tx.committed != allowed || tx.updated != allowed || !tx.rolledBack {
				t.Fatalf("err=%v committed=%v updated=%v rollback=%v", err, tx.committed, tx.updated, tx.rolledBack)
			}
			if !allowed && len(tx.events) != 0 {
				t.Fatal("forbidden cancellation wrote audit")
			}
			if allowed {
				want := "question.cancelled"
				if status == "internal_review" {
					want = "internal_review.cancelled," + want
				}
				if status == "committee_voting" {
					want = "committee_vote.cancelled," + want
				}
				if strings.Join(tx.events, ",") != want {
					t.Fatalf("audit=%v, want %s", tx.events, want)
				}
			}
		})
	}
	for _, status := range []string{"draft", "internal_review", "committee_voting"} {
		t.Run(status+" audit failure", func(t *testing.T) {
			tx := &cancellationTx{status: status, failAudit: true}
			err := (&application{}).cancelQuestionTransaction(context.Background(), tx, user{}, 1, "Причина")
			if err == nil || tx.committed || !tx.rolledBack {
				t.Fatal("audit failure must roll back without commit")
			}
		})
	}
}

func TestCancelQuestionRoleAndCSRF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := &application{sessionSecret: []byte("test-secret")}
	for _, role := range []string{"secretary", "admin", "approver", "committee", "observer"} {
		for _, validCSRF := range []bool{true, false} {
			router := gin.New()
			// An empty reason reaches the real handler without requiring a database.
			router.POST("/questions/:id/cancel", func(c *gin.Context) { c.Set("user", user{Role: role}) }, app.requireCSRF(), app.requireRole("secretary"), app.cancelQuestion)
			form := url.Values{}
			if validCSRF {
				form.Set("_csrf", app.csrfDigest("session", "test-session"))
			}
			request := httptest.NewRequest(http.MethodPost, "/questions/1/cancel", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.AddCookie(&http.Cookie{Name: "session_token", Value: "test-session"})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			want := http.StatusForbidden
			if role == "secretary" && validCSRF {
				want = http.StatusUnprocessableEntity
			}
			if response.Code != want {
				t.Fatalf("role=%s csrf=%v: status=%d want=%d", role, validCSRF, response.Code, want)
			}
		}
	}
}
