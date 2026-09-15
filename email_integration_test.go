//go:build mailintegration

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This suite uses only the dedicated temporary Unix socket, never PG* variables.
func TestEmailDatabaseIntegration(t *testing.T) {
	if os.Getenv("NEVA_MAIL_INTEGRATION") != "1" {
		t.Skip("set NEVA_MAIL_INTEGRATION=1 after starting the isolated test PostgreSQL")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig("host=/tmp/neva-smtp-test-socket dbname=postgres sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Password = ""
	cfg.ConnConfig.User = "boticelli"
	base, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	schema := fmt.Sprintf("mail_test_%d", time.Now().UnixNano())
	if _, err = base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := base.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	app := &application{db: db, sessionSecret: []byte("01234567890123456789012345678901"), loginLimiter: newLoginLimiter(100, time.Minute), mail: &mailConfig{Host: "127.0.0.1", Port: "1", Mode: "tls", From: "sender@example.com", Username: "sender@example.com", Password: "test-only", BaseURL: "https://example.com"}}
	if err = app.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = app.migrate(ctx); err != nil {
		t.Fatalf("repeated migration: %v", err)
	}
	passwordHash, err := hashPassword("test-password")
	if err != nil {
		t.Fatal(err)
	}
	create := func(email, role string) int64 {
		t.Helper()
		var id int64
		if err := db.QueryRow(ctx, `INSERT INTO users(email,full_name,password_hash,role) VALUES($1,$1,$2,$3) RETURNING id`, email, passwordHash, role).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	adminID := create("admin@example.com", "admin")
	memberID := create("member@example.com", "committee")
	session := func(id int64) string {
		t.Helper()
		token, err := randomToken(32)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,NOW()+INTERVAL '1 hour')`, app.sessionDigest(token), id); err != nil {
			t.Fatal(err)
		}
		return token
	}
	adminSession, memberSession := session(adminID), session(memberID)
	gin.SetMode(gin.TestMode)
	router := app.routes()
	post := func(path string, form url.Values, sessionToken string) *httptest.ResponseRecorder {
		if form == nil {
			form = url.Values{}
		}
		if sessionToken != "" {
			form.Set("_csrf", app.csrfDigest("session", sessionToken))
		} else {
			form.Set("_csrf", app.csrfDigest("email", "email-cookie"))
		}
		req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: "email_csrf", Value: "email-cookie"})
		if sessionToken != "" {
			req.AddCookie(&http.Cookie{Name: "session_token", Value: sessionToken})
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	latestToken := func(id int64) string {
		t.Helper()
		var encrypted []byte
		if err := db.QueryRow(ctx, `SELECT encrypted_body FROM email_outbox WHERE user_id=$1 AND token_id IS NOT NULL ORDER BY id DESC LIMIT 1`, id).Scan(&encrypted); err != nil {
			t.Fatal(err)
		}
		body, err := app.decryptMail(encrypted)
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(body, "/password/reset#")
		if len(parts) != 2 {
			t.Fatal("missing token")
		}
		return strings.Split(parts[1], "\n")[0]
	}
	setPassword := func(token string) *httptest.ResponseRecorder {
		return post("/password/reset", url.Values{"token": {token}, "password": {"new-test-password"}, "password_confirmation": {"new-test-password"}}, "")
	}

	t.Run("create and reset authorization", func(t *testing.T) {
		form := url.Values{"email": {"invited@example.com"}, "full_name": {"Invited"}, "role": {"observer"}}
		if w := post("/admin/users", form, memberSession); w.Code != 403 {
			t.Fatalf("forbidden create %d", w.Code)
		}
		if w := post("/admin/users", form, adminSession); w.Code != 303 {
			t.Fatalf("create %d: %s", w.Code, w.Body.String())
		}
		var invitedID int64
		if err := db.QueryRow(ctx, `SELECT id FROM users WHERE email='invited@example.com'`).Scan(&invitedID); err != nil {
			t.Fatal(err)
		}
		token := latestToken(invitedID)
		if w := setPassword(token); w.Code != 303 {
			t.Fatalf("accept invitation %d: %s", w.Code, w.Body.String())
		}
		if w := setPassword(token); w.Code != 422 {
			t.Fatalf("reused link %d", w.Code)
		}
		if w := post(fmt.Sprintf("/admin/users/%d/reset-password", invitedID), nil, memberSession); w.Code != 403 {
			t.Fatalf("forbidden reset %d", w.Code)
		}
		_ = session(invitedID)
		if w := post(fmt.Sprintf("/admin/users/%d/reset-password", invitedID), nil, adminSession); w.Code != 303 {
			t.Fatalf("admin reset %d: %s", w.Code, w.Body.String())
		}
		if count(`SELECT COUNT(*) FROM sessions WHERE user_id=$1`, invitedID) != 0 {
			t.Fatal("admin reset left sessions")
		}
		fresh := latestToken(invitedID)
		if fresh == token {
			t.Fatal("token reused")
		}
		if w := setPassword(fresh); w.Code != 303 {
			t.Fatalf("reset link %d", w.Code)
		}
	})
	t.Run("self service and concurrent one time use", func(t *testing.T) {
		known := post("/password/forgot", url.Values{"email": {"member@example.com"}}, "")
		unknown := post("/password/forgot", url.Values{"email": {"unknown@example.com"}}, "")
		// Cookies are random, but the account-disclosure message and status must match.
		expected := "Если такой активный пользователь существует"
		if known.Code != 200 || unknown.Code != 200 || !strings.Contains(known.Body.String(), expected) || !strings.Contains(unknown.Body.String(), expected) {
			t.Fatal("account existence exposed or request failed")
		}
		if count(`SELECT COUNT(*) FROM email_outbox WHERE user_id=$1`, memberID) != 1 {
			t.Fatal("missing reset email")
		}
		post("/password/forgot", url.Values{"email": {"member@example.com"}}, "")
		if count(`SELECT COUNT(*) FROM email_outbox WHERE user_id=$1`, memberID) != 1 {
			t.Fatal("per-account cooldown not applied")
		}
		token := latestToken(memberID)
		var wg sync.WaitGroup
		codes := make(chan int, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); codes <- setPassword(token).Code }()
		}
		wg.Wait()
		close(codes)
		successful := 0
		for code := range codes {
			if code == 303 {
				successful++
			} else if code != 422 {
				t.Fatalf("concurrent reset returned %d", code)
			}
		}
		if successful != 1 {
			t.Fatalf("accepted %d times", successful)
		}
		if count(`SELECT COUNT(*) FROM sessions WHERE user_id=$1`, memberID) != 0 {
			t.Fatal("self reset left sessions")
		}
	})
	t.Run("expired and deactivated links", func(t *testing.T) {
		for _, mode := range []string{"expired", "deactivated", "password-changed"} {
			id := create(mode+"@example.com", "observer")
			tx, err := db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = app.issuePasswordEmail(ctx, tx, id, false); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			token := latestToken(id)
			switch mode {
			case "expired":
				_, err = db.Exec(ctx, `UPDATE email_password_tokens SET expires_at=NOW()-INTERVAL '1 second' WHERE user_id=$1`, id)
			case "deactivated":
				_, err = db.Exec(ctx, `UPDATE users SET active=FALSE WHERE id=$1`, id)
			case "password-changed":
				_, err = db.Exec(ctx, `UPDATE users SET password_hash='changed' WHERE id=$1`, id)
			}
			if err != nil {
				t.Fatal(err)
			}
			if w := setPassword(token); w.Code != 422 {
				t.Fatalf("%s link accepted: %d", mode, w.Code)
			}
		}
	})
	t.Run("workflow recipients and transactional outbox", func(t *testing.T) {
		secretaryID := create("secretary@example.com", "secretary")
		_ = secretaryID
		approverID := create("approver@example.com", "approver")
		_ = approverID
		var qid, rid int64
		if err := db.QueryRow(ctx, `INSERT INTO questions(question_type,title,decision_text,internal_deadline,status,created_by) VALUES('other','Test question','Decision',CURRENT_DATE,'committee_voting',$1) RETURNING id`, adminID).Scan(&qid); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(ctx, `INSERT INTO committee_vote_rounds(question_id,deadline,frozen_decision_text,roster_size,started_by) VALUES($1,CURRENT_DATE,'Decision',1,$2) RETURNING id`, qid, adminID).Scan(&rid); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO committee_vote_participants(round_id,user_id,name_snapshot,email_snapshot) VALUES($1,$2,'Member','member@example.com')`, rid, memberID); err != nil {
			t.Fatal(err)
		}
		actor := user{ID: adminID, Role: "admin", FullName: "Admin", Email: "admin@example.com"}
		record := auditRecord{EventType: "committee_vote.started", QuestionID: &qid, TargetType: "question", TargetID: &qid}
		before := count(`SELECT COUNT(*) FROM email_outbox`)
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = app.writeAudit(ctx, tx, actor, record); err != nil {
			t.Fatal(err)
		}
		if err = tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if count(`SELECT COUNT(*) FROM email_outbox`) != before {
			t.Fatal("rolled back event sent mail")
		}
		tx, err = db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = app.writeAudit(ctx, tx, actor, record); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if count(`SELECT COUNT(*) FROM email_outbox WHERE token_id IS NULL`) != 2 {
			t.Fatal("workflow must notify secretary and actual participant only")
		}
	})
	t.Run("queue cancellation and SMTP retry", func(t *testing.T) {
		// All password links above were used, expired, deactivated or superseded.
		for i := 0; i < 20; i++ {
			worked, err := app.deliverNextMail(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !worked {
				break
			}
		}
		if count(`SELECT COUNT(*) FROM email_outbox WHERE token_id IS NOT NULL AND completed_at IS NULL`) != 0 {
			t.Fatal("stale token emails remain deliverable")
		}
		if count(`SELECT COUNT(*) FROM email_outbox WHERE outcome='retry' AND next_attempt_at>NOW()`) != 2 {
			t.Fatal("SMTP failure did not schedule retries")
		}
		if count(`SELECT COUNT(*) FROM email_outbox WHERE completed_at IS NOT NULL AND encrypted_body IS NOT NULL`) != 0 {
			t.Fatal("completed queue retained secret payload")
		}
	})
	if err := app.migrate(ctx); err != nil {
		t.Fatalf("migration over populated database: %v", err)
	}
}
