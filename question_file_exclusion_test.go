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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestFileExclusionReason(t *testing.T) {
	for _, reason := range []string{"", " \t\n", strings.Repeat("я", 2001)} {
		if _, err := validateFileExclusionReason(reason); err == nil {
			t.Fatal("accepted invalid reason")
		}
	}
	got, err := validateFileExclusionReason(" \n" + strings.Repeat("я", 2000) + " ")
	if err != nil || len([]rune(got)) != 2000 {
		t.Fatal("valid reason must be trimmed and accepted")
	}
}

type exclusionTestTx struct {
	pgx.Tx
	status, questionType, fileStatus               string
	version                                        int
	active, pending, stale, missingFile, failAudit bool
	remaining                                      []decisionTestRow
	committed, rolledBack, excluded, reset         bool
	auditArgs                                      []any
}

func (tx *exclusionTestTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "SELECT status, question_type"):
		if !strings.HasSuffix(sql, "FOR UPDATE") {
			return cancellationRow(func(...any) error { return errors.New("question must be locked") })
		}
		now := decisionTestTime
		if tx.stale {
			now = now.Add(time.Second)
		}
		return decisionTestRow{tx.status, tx.questionType, now}
	case strings.Contains(sql, "SELECT title, status"):
		if !strings.Contains(sql, "question_id = $2 FOR UPDATE") {
			return cancellationRow(func(...any) error { return errors.New("file must belong to question and be locked") })
		}
		if tx.missingFile || args[0] != int64(5) || args[1] != int64(1) {
			return cancellationRow(func(...any) error { return pgx.ErrNoRows })
		}
		return decisionTestRow{"Материал", tx.fileStatus, tx.version}
	case strings.Contains(sql, "FROM question_file_versions"):
		return decisionTestRow{tx.pending}
	case strings.Contains(sql, "FROM internal_review_rounds"):
		return decisionTestRow{tx.active}
	default:
		return cancellationRow(func(...any) error { return errors.New("unexpected query") })
	}
}

func (tx *exclusionTestTx) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if !strings.Contains(sql, "id <> $2 AND status = 'active' AND current_version_no IS NOT NULL") {
		return nil, errors.New("remaining bundle must contain only other official active files")
	}
	return &decisionTestRows{rows: tx.remaining}, nil
}

func (tx *exclusionTestTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "UPDATE question_files SET status = 'excluded'"):
		tx.excluded = true
	case strings.Contains(sql, "UPDATE questions SET status = 'draft'"):
		tx.reset = true
	case strings.Contains(sql, "INSERT INTO audit_events"):
		if tx.failAudit {
			return pgconn.CommandTag{}, errors.New("audit failed")
		}
		tx.auditArgs = args
	default:
		return pgconn.CommandTag{}, errors.New("unexpected mutation: " + sql)
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (tx *exclusionTestTx) Commit(context.Context) error   { tx.committed = true; return nil }
func (tx *exclusionTestTx) Rollback(context.Context) error { tx.rolledBack = true; return nil }

func TestFileExclusionTransaction(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*exclusionTestTx)
		wantErr bool
	}{
		{name: "draft"},
		{name: "ready resets review", change: func(tx *exclusionTestTx) { tx.status = "ready_for_committee" }},
		{name: "revision resets review", change: func(tx *exclusionTestTx) { tx.status = "revision_required" }},
		{name: "rejected committee", change: func(tx *exclusionTestTx) { tx.status = "rejected" }},
		{name: "no quorum", change: func(tx *exclusionTestTx) { tx.status = "no_quorum" }},
		{name: "rejected upload without official version", change: func(tx *exclusionTestTx) { tx.version = 0; tx.remaining = nil }},
		{name: "active review", change: func(tx *exclusionTestTx) { tx.status = "internal_review" }, wantErr: true},
		{name: "active committee", change: func(tx *exclusionTestTx) { tx.status = "committee_voting" }, wantErr: true},
		{name: "active round despite draft", change: func(tx *exclusionTestTx) { tx.active = true }, wantErr: true},
		{name: "approved", change: func(tx *exclusionTestTx) { tx.status = "approved" }, wantErr: true},
		{name: "cancelled", change: func(tx *exclusionTestTx) { tx.status = "cancelled" }, wantErr: true},
		{name: "excluded already", change: func(tx *exclusionTestTx) { tx.fileStatus = "excluded" }, wantErr: true},
		{name: "foreign file", change: func(tx *exclusionTestTx) { tx.missingFile = true }, wantErr: true},
		{name: "pending version", change: func(tx *exclusionTestTx) { tx.pending = true }, wantErr: true},
		{name: "stale page", change: func(tx *exclusionTestTx) { tx.stale = true }, wantErr: true},
		{name: "last official file", change: func(tx *exclusionTestTx) { tx.remaining = nil }, wantErr: true},
		{name: "missing mandatory contract", change: func(tx *exclusionTestTx) {
			tx.questionType = "transaction"
			tx.remaining = []decisionTestRow{{int64(7), 1, "terms_summary"}}
		}, wantErr: true},
		{name: "missing mandatory terms", change: func(tx *exclusionTestTx) {
			tx.questionType = "transaction"
			tx.remaining = []decisionTestRow{{int64(7), 1, "contract"}}
		}, wantErr: true},
		{name: "contract bundle retained", change: func(tx *exclusionTestTx) {
			tx.questionType = "transaction"
			tx.remaining = []decisionTestRow{{int64(7), 1, "contract"}, {int64(8), 1, "terms_summary"}}
		}},
		{name: "missing LNA", change: func(tx *exclusionTestTx) { tx.questionType = "internal_document" }, wantErr: true},
		{name: "LNA retained", change: func(tx *exclusionTestTx) {
			tx.questionType = "internal_document"
			tx.remaining = []decisionTestRow{{int64(7), 1, "lna_draft"}}
		}},
		{name: "audit failure", change: func(tx *exclusionTestTx) { tx.failAudit = true }, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx := &exclusionTestTx{status: "draft", questionType: "budget", fileStatus: "active", version: 2, remaining: []decisionTestRow{{int64(7), 1, "other"}}}
			if test.change != nil {
				test.change(tx)
			}
			err := (&application{}).excludeQuestionFileTransaction(context.Background(), tx, user{ID: 3, Role: "secretary"}, 1, 5, "Дублирующий материал", decisionTestTime)
			if (err != nil) != test.wantErr || tx.committed == test.wantErr || !tx.rolledBack {
				t.Fatalf("err=%v commit=%v rollback=%v", err, tx.committed, tx.rolledBack)
			}
			if test.wantErr && !tx.failAudit && (tx.excluded || tx.reset) {
				t.Fatal("invalid exclusion changed state")
			}
			if !test.wantErr && (!tx.excluded || !tx.reset || tx.auditArgs[4] != "question.file_excluded") {
				t.Fatal("exclusion must reset review and write audit")
			}
		})
	}
}

func TestFileExclusionRoleAndCSRF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := &application{sessionSecret: []byte("exclusion-test")}
	for _, role := range []string{"secretary", "admin", "approver", "committee", "observer"} {
		for _, csrf := range []bool{true, false} {
			router := gin.New()
			router.POST("/questions/:id/files/:fileID/exclude", func(c *gin.Context) { c.Set("user", user{Role: role}) }, app.requireCSRF(), app.requireRole("secretary"), app.excludeQuestionFile)
			form := url.Values{}
			if csrf {
				form.Set("_csrf", app.csrfDigest("session", "exclusion-session"))
			}
			request := httptest.NewRequest(http.MethodPost, "/questions/1/files/5/exclude", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.AddCookie(&http.Cookie{Name: "session_token", Value: "exclusion-session"})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			want := 403
			if role == "secretary" && csrf {
				want = 422
			}
			if response.Code != want {
				t.Fatalf("role=%s csrf=%v: status=%d", role, csrf, response.Code)
			}
		}
	}
}

func TestExcludedFileHistoryTemplate(t *testing.T) {
	tmpl, err := template.ParseFiles("templates/question.html")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = tmpl.Execute(&output, gin.H{
		"User": user{Role: "secretary"}, "Question": questionDetail{ID: 1}, "CanUploadFiles": true,
		"RevisionPlan": revisionPlanView{}, "CommitteeVote": committeeVoteView{}, "InternalReview": internalReviewView{},
		"Files": []questionFileItem{{ID: 5, Excluded: true, ExclusionReason: "<script>Причина</script>", Versions: []questionFileVersionItem{{VersionNo: 1, Filename: "История.pdf"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := output.String()
	for _, expected := range []string{"Исключён из комплекта", "&lt;script&gt;Причина&lt;/script&gt;", "/files/5/versions/1/download"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("missing: %s", expected)
		}
	}
	for _, forbidden := range []string{"<script>", `action="/questions/1/files/5/versions"`, "/files/5/exclude", "/files/5/versions/1/confirm"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("excluded history must not offer mutations: %s", forbidden)
		}
	}
}
