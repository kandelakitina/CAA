package main

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var decisionTestTime = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func TestDecisionRevisionHistoryTemplate(t *testing.T) {
	tmpl, err := template.ParseFiles("templates/question.html", "templates/navigation.html")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = tmpl.Execute(&output, gin.H{
		"User": user{Role: "observer"}, "Question": questionDetail{ID: 1},
		"RevisionPlan": revisionPlanView{}, "CommitteeVote": committeeVoteView{},
		"DecisionRevisions": []decisionRevisionItem{{Number: 1, PreviousText: "Старый текст", Text: "<script>Новый текст</script>", Reason: "Причина"}},
		"InternalReview":    internalReviewView{HasRound: true, Items: []internalReviewItem{{HasVisa: true, Visa: internalVisaItem{CarryLabel: "Исходный раунд №10", SourceDecisionText: "Исходная формулировка"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Старый текст", "&lt;script&gt;Новый текст&lt;/script&gt;", "Исходная формулировка", "В старом раунде формулировка отдельно не фиксировалась."} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing text: %s", expected)
		}
	}
	if strings.Contains(output.String(), "<script>") || strings.Contains(output.String(), "/decision-revisions/new") {
		t.Fatal("history must be escaped and read-only for observer")
	}
}

func validDecisionRevisionInput() decisionRevisionInput {
	return decisionRevisionInput{Text: "Новая формулировка", Reason: "Уточнение", Policy: "carry_positive", Deadline: "2026-09-10", Token: decisionTestTime.Format(time.RFC3339Nano)}
}

func TestCommitteeStartRequiresCurrentContext(t *testing.T) {
	for _, token := range []string{"", "invalid", decisionTestTime.Add(-time.Second).Format(time.RFC3339Nano)} {
		if committeeContextMatches(token, decisionTestTime) {
			t.Fatal("missing or stale context must block Committee start")
		}
	}
	if !committeeContextMatches(decisionTestTime.Format(time.RFC3339Nano), decisionTestTime) {
		t.Fatal("current context must be accepted")
	}
}

func TestDecisionRevisionCarryPolicy(t *testing.T) {
	files := []revisionFile{{ID: 1, CurrentVersion: 2}}
	sources := map[string]revisionSourceVisa{
		"1:legal":        {VisaID: 1, VersionNo: 2, Decision: "approved"},
		"1:finance":      {VisaID: 2, VersionNo: 2, Decision: "approved_with_comments"},
		"1:construction": {VisaID: 3, VersionNo: 2, Decision: "no_comments_without_review"},
		"1:security":     {VisaID: 4, VersionNo: 1, Decision: "rejected"},
	}
	carry, err := planDecisionRevision(files, sources, "carry_positive")
	if err != nil || len(carry) != 3 {
		t.Fatalf("carry=%v err=%v", carry, err)
	}
	carry, err = planDecisionRevision(files, sources, "recheck_all")
	if err != nil || len(carry) != 0 {
		t.Fatal("recheck all must not carry visas")
	}
	carry, err = planDecisionRevision(files, nil, "carry_positive")
	if err != nil || len(carry) != 0 {
		t.Fatal("missing visas must not be invented")
	}
}

func TestDecisionRevisionValidation(t *testing.T) {
	for _, change := range []func(*decisionRevisionInput){
		func(i *decisionRevisionInput) { i.Text = " \n" },
		func(i *decisionRevisionInput) { i.Text = strings.Repeat("я", 10001) },
		func(i *decisionRevisionInput) { i.Reason = " " },
		func(i *decisionRevisionInput) { i.Reason = strings.Repeat("я", 2001) },
		func(i *decisionRevisionInput) { i.Policy = "" },
		func(i *decisionRevisionInput) { i.Policy = "carry_rejected" },
		func(i *decisionRevisionInput) { i.Deadline = "2026-09-07" },
		func(i *decisionRevisionInput) { i.Deadline = "invalid" },
		func(i *decisionRevisionInput) { i.Token = "" },
	} {
		input := validDecisionRevisionInput()
		change(&input)
		if _, _, err := validateDecisionRevision(input, decisionTestTime); err == nil {
			t.Fatalf("accepted invalid input: %+v", input)
		}
	}
	input := validDecisionRevisionInput()
	input.Text, input.Reason = strings.Repeat("я", 10000), strings.Repeat("я", 2000)
	if _, _, err := validateDecisionRevision(input, decisionTestTime); err != nil {
		t.Fatal(err)
	}
}

type decisionTestRow []any

func (r decisionTestRow) Scan(dest ...any) error {
	if len(r) != len(dest) {
		return errors.New("unexpected scan length")
	}
	for i := range dest {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(r[i]))
	}
	return nil
}

type decisionTestRows struct {
	pgx.Rows
	rows  []decisionTestRow
	index int
}

func (r *decisionTestRows) Next() bool             { r.index++; return r.index <= len(r.rows) }
func (r *decisionTestRows) Scan(dest ...any) error { return r.rows[r.index-1].Scan(dest...) }
func (r *decisionTestRows) Close()                 {}
func (r *decisionTestRows) Err() error             { return nil }

type decisionTestTx struct {
	pgx.Tx
	excludedSinceRound                                                                  bool
	status                                                                              string
	active, noHistory, missing, stale, rejected, changedFile, missingService, failAudit bool
	pendingUploads, pendingRequirements, carried                                        int
	committed, rolledBack, completed                                                    bool
	writes                                                                              int
	snapshot, oldText, newText                                                          string
	events                                                                              []string
}

func (tx *decisionTestTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "SELECT title, decision_text"):
		if tx.missing {
			return cancellationRow(func(...any) error { return pgx.ErrNoRows })
		}
		if !strings.HasSuffix(sql, "FOR UPDATE") {
			return cancellationRow(func(...any) error { return errors.New("question must be locked") })
		}
		updated := decisionTestTime
		if tx.stale {
			updated = updated.Add(time.Second)
		}
		return decisionTestRow{"Вопрос", "Прежняя формулировка", tx.status, "budget", updated}
	case strings.Contains(sql, "f.excluded_at"):
		return decisionTestRow{tx.excludedSinceRound}
	case strings.Contains(sql, "SELECT EXISTS"):
		return decisionTestRow{tx.active}
	case strings.Contains(sql, "SELECT id FROM internal_review_rounds"):
		if tx.noHistory {
			return cancellationRow(func(...any) error { return pgx.ErrNoRows })
		}
		return decisionTestRow{int64(10)}
	case strings.Contains(sql, "COUNT(*) FILTER"):
		return decisionTestRow{tx.pendingRequirements, 0}
	case strings.Contains(sql, "SELECT COUNT(*)"):
		return decisionTestRow{tx.pendingUploads}
	case strings.Contains(sql, "INSERT INTO internal_review_rounds"):
		tx.writes++
		tx.snapshot = args[3].(string)
		return decisionTestRow{int64(11)}
	case strings.Contains(sql, "INSERT INTO internal_review_requirements"):
		tx.writes++
		if args[4] == "pending" {
			tx.pendingRequirements++
		}
		return decisionTestRow{int64(100 + tx.writes)}
	case strings.Contains(sql, "INSERT INTO decision_text_revisions"):
		tx.writes++
		tx.oldText, tx.newText = args[1].(string), args[2].(string)
		return decisionTestRow{1}
	default:
		return cancellationRow(func(...any) error { return errors.New("unexpected QueryRow: " + sql) })
	}
}

func (tx *decisionTestTx) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	version := 1
	if tx.changedFile {
		version = 2
	}
	var rows []decisionTestRow
	switch {
	case strings.Contains(sql, "COALESCE(v.source_visa_id"):
		for i, service := range requiredInternalServices {
			decision := "approved"
			if tx.rejected && i == 0 {
				decision = "rejected"
			}
			rows = append(rows, decisionTestRow{int64(i + 1), int64(5), 1, service, decision, "Комментарий", int64(20 + i), decisionTestTime})
		}
	case strings.Contains(sql, "FROM users"):
		for i, service := range requiredInternalServices {
			if tx.missingService && i == 0 {
				continue
			}
			rows = append(rows, decisionTestRow{service, 1})
		}
	case strings.Contains(sql, "current_version_no, category"):
		rows = []decisionTestRow{{int64(5), version, "other"}}
	case strings.Contains(sql, "FROM question_files"):
		rows = []decisionTestRow{{int64(5), "Материал", version}}
	default:
		return nil, errors.New("unexpected Query: " + sql)
	}
	return &decisionTestRows{rows: rows}, nil
}

func (tx *decisionTestTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tx.writes++
	if strings.Contains(sql, "INSERT INTO audit_events") {
		if tx.failAudit {
			return pgconn.CommandTag{}, errors.New("audit failed")
		}
		tx.events = append(tx.events, args[4].(string))
	}
	if strings.Contains(sql, "INSERT INTO internal_review_visas") {
		tx.carried++
	}
	if strings.Contains(sql, "UPDATE internal_review_rounds SET status = 'completed'") {
		tx.completed = true
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (tx *decisionTestTx) Commit(context.Context) error   { tx.committed = true; return nil }
func (tx *decisionTestTx) Rollback(context.Context) error { tx.rolledBack = true; return nil }

func TestDecisionRevisionTransaction(t *testing.T) {
	for _, test := range []struct {
		name                     string
		tx                       decisionTestTx
		policy                   string
		wantErr                  bool
		wantCarried, wantPending int
	}{
		{name: "carry all auto completes", tx: decisionTestTx{status: "ready_for_committee"}, policy: "carry_positive", wantCarried: 4},
		{name: "recheck all stays active", tx: decisionTestTx{status: "ready_for_committee"}, policy: "recheck_all", wantPending: 4},
		{name: "changed files not carried", tx: decisionTestTx{status: "revision_required", changedFile: true, rejected: true}, policy: "carry_positive", wantPending: 4},
		{name: "draft after cancellation", tx: decisionTestTx{status: "draft"}, policy: "recheck_all", wantPending: 4},
		{name: "rejected committee result", tx: decisionTestTx{status: "rejected"}, policy: "carry_positive", wantCarried: 4},
		{name: "no quorum", tx: decisionTestTx{status: "no_quorum"}, policy: "carry_positive", wantCarried: 4},
		{name: "active review", tx: decisionTestTx{status: "internal_review", active: true}, wantErr: true},
		{name: "active committee", tx: decisionTestTx{status: "committee_voting", active: true}, wantErr: true},
		{name: "approved", tx: decisionTestTx{status: "approved"}, wantErr: true},
		{name: "cancelled", tx: decisionTestTx{status: "cancelled"}, wantErr: true},
		{name: "fresh draft", tx: decisionTestTx{status: "draft", noHistory: true}, wantErr: true},
		{name: "missing", tx: decisionTestTx{missing: true}, wantErr: true},
		{name: "stale", tx: decisionTestTx{status: "ready_for_committee", stale: true}, wantErr: true},
		{name: "unrevised rejection", tx: decisionTestTx{status: "revision_required", rejected: true}, wantErr: true},
		{name: "pending uploads", tx: decisionTestTx{status: "ready_for_committee", pendingUploads: 1}, wantErr: true},
		{name: "missing service", tx: decisionTestTx{status: "ready_for_committee", missingService: true}, wantErr: true},
		{name: "audit failure", tx: decisionTestTx{status: "ready_for_committee", failAudit: true}, wantErr: true},
		{name: "exclusion blocks old visa carry", tx: decisionTestTx{status: "draft", excludedSinceRound: true}, policy: "carry_positive", wantErr: true},
		{name: "exclusion permits full recheck", tx: decisionTestTx{status: "draft", excludedSinceRound: true}, policy: "recheck_all", wantPending: 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := validDecisionRevisionInput()
			if test.policy != "" {
				input.Policy = test.policy
			}
			tx := &test.tx
			err := (&application{}).createDecisionRevisionTransaction(context.Background(), tx, user{ID: 2, FullName: "Секретарь"}, 1, input, decisionTestTime)
			if (err != nil) != test.wantErr || tx.committed == test.wantErr || !tx.rolledBack {
				t.Fatalf("err=%v committed=%v rollback=%v", err, tx.committed, tx.rolledBack)
			}
			if test.wantErr && !tx.failAudit && tx.writes != 0 {
				t.Fatal("invalid request wrote data")
			}
			if !test.wantErr {
				if tx.carried != test.wantCarried || tx.pendingRequirements != test.wantPending || tx.completed != (test.wantPending == 0) {
					t.Fatalf("carried=%d pending=%d complete=%v", tx.carried, tx.pendingRequirements, tx.completed)
				}
				if tx.oldText != "Прежняя формулировка" || tx.newText != input.Text || tx.snapshot != input.Text {
					t.Fatal("missing immutable text snapshots")
				}
				if len(tx.events) < 2 || tx.events[0] != "question.decision_revised" {
					t.Fatal("missing audit")
				}
			}
		})
	}
}

func TestDecisionRevisionRolesAndCSRF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := &application{sessionSecret: []byte("revision-test")}
	for _, role := range []string{"secretary", "admin", "approver", "committee", "observer"} {
		for _, csrf := range []bool{true, false} {
			router := gin.New()
			router.LoadHTMLGlob("templates/*")
			setUser := func(c *gin.Context) { c.Set("user", user{Role: role}) }
			router.GET("/questions/:id/decision-revisions/new", setUser, app.requireRole("admin", "secretary"), app.showDecisionRevision)
			router.POST("/questions/:id/decision-revisions", setUser, app.requireCSRF(), app.requireRole("admin", "secretary"), app.createDecisionRevision)
			get := httptest.NewRecorder()
			router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/questions/invalid/decision-revisions/new", nil))
			wantGet := 403
			if role == "secretary" || role == "admin" {
				wantGet = 400
			}
			if get.Code != wantGet {
				t.Fatalf("GET %s: %d", role, get.Code)
			}
			form := url.Values{"decision_text": {"<script>Сохранённый ввод</script>"}}
			if csrf {
				form.Set("_csrf", app.csrfDigest("session", "revision-session"))
			}
			request := httptest.NewRequest(http.MethodPost, "/questions/1/decision-revisions", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.AddCookie(&http.Cookie{Name: "session_token", Value: "revision-session"})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			want := 403
			if (role == "secretary" || role == "admin") && csrf {
				want = 422
			}
			if response.Code != want {
				t.Fatalf("role=%s csrf=%v: status=%d", role, csrf, response.Code)
			}
			if want == 422 && (!strings.Contains(response.Body.String(), "&lt;script&gt;") || strings.Contains(response.Body.String(), "<script>")) {
				t.Fatal("input must be retained and escaped")
			}
		}
	}
}
