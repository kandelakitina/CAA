//go:build localintegration

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

type localBrowser struct {
	t      *testing.T
	app    *application
	base   string
	client *http.Client
}

func (b *localBrowser) request(method, path string, fields url.Values, files [][]byte, want int) (string, string) {
	b.t.Helper()
	var body io.Reader
	contentType := "application/x-www-form-urlencoded"
	if method == http.MethodPost {
		if fields == nil {
			fields = url.Values{}
		}
		u, _ := url.Parse(b.base)
		for _, cookie := range b.client.Jar.Cookies(u) {
			if cookie.Name == "session_token" {
				fields.Set("_csrf", b.app.csrfDigest("session", cookie.Value))
			}
		}
		if files == nil {
			body = strings.NewReader(fields.Encode())
		} else {
			var buffer bytes.Buffer
			writer := multipart.NewWriter(&buffer)
			for key, values := range fields {
				for _, value := range values {
					if err := writer.WriteField(key, value); err != nil {
						b.t.Fatal(err)
					}
				}
			}
			field := "attachments"
			if strings.Contains(path, "/files") || strings.HasPrefix(path, "/documents") {
				field = "file"
			}
			for _, file := range files {
				part, err := writer.CreateFormFile(field, "material.pdf")
				if err != nil {
					b.t.Fatal(err)
				}
				if _, err := part.Write(file); err != nil {
					b.t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				b.t.Fatal(err)
			}
			body, contentType = &buffer, writer.FormDataContentType()
		}
	}
	req, err := http.NewRequest(method, b.base+path, body)
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal("local HTTP request failed")
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Fatal(err)
	}
	if resp.StatusCode != want {
		b.t.Fatalf("%s %s: status %d, expected %d", method, path, resp.StatusCode, want)
	}
	return string(payload), resp.Header.Get("Location")
}

func TestLocalIntegration(t *testing.T) {
	if os.Getenv("NEVA_LOCAL_INTEGRATION") != "1" {
		t.Skip("run scripts/local-test.ps1 -Action Test")
	}
	for key, expected := range map[string]string{"PGHOST": "127.0.0.1", "PGPORT": "55432", "PGDATABASE": "neva_local_test", "PGUSER": "neva_local_test", "S3_ENDPOINT": "http://127.0.0.1:18333", "S3_BUCKET": "neva-local-test"} {
		if os.Getenv(key) != expected {
			t.Fatalf("refusing non-local test configuration: %s", key)
		}
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig("host=127.0.0.1 port=55432 dbname=neva_local_test user=neva_local_test sslmode=disable")
	if err != nil {
		t.Fatal("invalid local database configuration")
	}
	cfg.ConnConfig.Password = os.Getenv("PGPASSWORD")
	bootstrap, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("open local database")
	}
	runID, err := randomToken(8)
	if err != nil {
		t.Fatal(err)
	}
	// Every run owns a fresh schema; prior test history is retained, never truncated.
	schema := "it_" + fmt.Sprintf("%x", []byte(runID))
	if _, err := bootstrap.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	bootstrap.Close()
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("open isolated schema")
	}
	defer db.Close()
	app := &application{db: db, sessionSecret: []byte(os.Getenv("SESSION_SECRET")), loginLimiter: newLoginLimiter(5, 15*time.Minute)}
	app.storage, err = newStorage(ctx)
	if err != nil {
		t.Fatal("configure local S3")
	}
	if err := app.migrate(ctx); err != nil {
		t.Fatalf("initial migration: %v", err)
	}
	if err := app.migrate(ctx); err != nil {
		t.Fatalf("repeated migration: %v", err)
	}
	_, err = app.storage.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &app.storage.bucket})
	if err != nil {
		if _, err := app.storage.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &app.storage.bucket}); err != nil {
			t.Fatal("create local bucket")
		}
	}
	password, err := randomToken(24)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("INITIAL_ADMIN_EMAIL", "admin@local.test")
	t.Setenv("INITIAL_ADMIN_PASSWORD", password)
	if err := app.createInitialAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	server := httptest.NewTLSServer(app.routes())
	defer server.Close()
	newBrowser := func() *localBrowser {
		clientCopy := *server.Client()
		client := &clientCopy
		jar, _ := cookiejar.New(nil)
		client.Jar, client.Timeout = jar, 30*time.Second
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		return &localBrowser{t: t, app: app, base: server.URL, client: client}
	}
	login := func(email string, change bool) *localBrowser {
		b := newBrowser()
		html, _ := b.request("GET", "/login", nil, nil, 200)
		match := regexp.MustCompile(`name="_csrf" value="([^"]+)"`).FindStringSubmatch(html)
		if len(match) != 2 {
			t.Fatal("login CSRF field missing")
		}
		_, location := b.request("POST", "/login", url.Values{"email": {email}, "password": {password}, "_csrf": {match[1]}}, nil, 303)
		if change {
			if location != "/password/change" {
				t.Fatal("new user must change password")
			}
			b.request("POST", "/password/change", url.Values{"current_password": {password}, "new_password": {password + "-changed"}, "password_confirmation": {password + "-changed"}}, nil, 303)
		}
		return b
	}
	admin := login("admin@local.test", false)
	users := map[string]*localBrowser{}
	for _, role := range []string{"secretary", "observer", "legal", "finance", "construction", "security", "chair", "member"} {
		fields := url.Values{"email": {role + "@local.test"}, "full_name": {role}, "role": {role}, "temporary_password": {password}, "password_confirmation": {password}}
		if validInternalService(role) {
			fields.Set("role", "approver")
			fields.Set("internal_service", role)
		}
		if role == "chair" || role == "member" {
			fields.Set("role", "committee")
		}
		if role == "chair" {
			fields.Set("is_committee_chair", "1")
		}
		admin.request("POST", "/admin/users", fields, nil, 303)
		users[role] = login(role+"@local.test", true)
	}
	secretary := users["secretary"]
	deadline := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	_, question := secretary.request("POST", "/questions", url.Values{"question_type": {"budget"}, "title": {"Local integration " + runID}, "decision_text": {"Approve test budget"}, "internal_deadline": {deadline}}, nil, 303)
	qid, err := strconv.ParseInt(strings.TrimPrefix(question, "/questions/"), 10, 64)
	if err != nil {
		t.Fatal("missing question redirect")
	}
	newBrowser().request("GET", question, nil, nil, 303)
	users["chair"].request("GET", question, nil, nil, 404)
	users["observer"].request("POST", question+"/internal-review/start", url.Values{"deadline": {deadline}}, nil, 403)
	// A logged-in request without CSRF is refused by the production middleware.
	req, _ := http.NewRequest("POST", server.URL+question+"/internal-review/start", strings.NewReader("deadline="+deadline))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	csrfResponse, err := secretary.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	csrfResponse.Body.Close()
	if csrfResponse.StatusCode != 403 {
		t.Fatal("missing CSRF was accepted")
	}
	pdf := []byte("%PDF-1.7\nlocal integration document\n%%EOF\n")
	secretary.request("POST", question+"/files", url.Values{"title": {"Budget"}, "category": {"other"}}, [][]byte{pdf}, 303)
	var fileID int64
	if err := db.QueryRow(ctx, "SELECT id FROM question_files WHERE question_id=$1", qid).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	filePath := fmt.Sprintf("%s/files/%d/versions", question, fileID)
	download, _ := secretary.request("GET", filePath+"/1/download", nil, nil, 200)
	if download != string(pdf) {
		t.Fatal("S3 downloaded bytes differ")
	}
	var objectKey string
	if err := db.QueryRow(ctx, "SELECT object_key FROM question_file_versions WHERE question_file_id=$1 AND version_no=1", fileID).Scan(&objectKey); err != nil {
		t.Fatal(err)
	}
	anon, err := http.Get(os.Getenv("S3_ENDPOINT") + "/" + app.storage.bucket + "/" + objectKey)
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != 403 {
		t.Fatalf("unsigned S3 download returned %d", anon.StatusCode)
	}
	status := func(want string) {
		t.Helper()
		var got string
		if err := db.QueryRow(ctx, "SELECT status FROM questions WHERE id=$1", qid).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("question state %s, expected %s", got, want)
		}
	}
	token := func() string {
		var updated time.Time
		if err := db.QueryRow(ctx, "SELECT updated_at FROM questions WHERE id=$1", qid).Scan(&updated); err != nil {
			t.Fatal(err)
		}
		return updated.Format(time.RFC3339Nano)
	}
	visa := func(service, decision string) {
		var requirement int64
		if err := db.QueryRow(ctx, `SELECT r.id FROM internal_review_requirements r JOIN internal_review_rounds rr ON rr.id=r.round_id WHERE rr.question_id=$1 AND rr.status='active' AND r.internal_service=$2 AND r.status='pending'`, qid, service).Scan(&requirement); err != nil {
			t.Fatal(err)
		}
		var attachments [][]byte
		if service == "legal" && decision == "rejected" {
			large := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte(" "), 14<<20)...)
			large = append(large, []byte("\n%%EOF\n")...)
			attachments = [][]byte{large, large}
		}
		users[service].request("POST", question+"/internal-review/respond", url.Values{"requirement_id": {strconv.FormatInt(requirement, 10)}, "decision": {decision}, "comment": {"Integration review"}}, attachments, 303)
	}
	// Two real HTTP requests compete for the question lock. Exactly one starts.
	startForm := url.Values{"deadline": {deadline}}
	baseURL, _ := url.Parse(server.URL)
	for _, cookie := range secretary.client.Jar.Cookies(baseURL) {
		if cookie.Name == "session_token" {
			startForm.Set("_csrf", app.csrfDigest("session", cookie.Value))
		}
	}
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r, _ := http.NewRequest("POST", server.URL+question+"/internal-review/start", strings.NewReader(startForm.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response, err := secretary.client.Do(r)
			if err != nil {
				codes <- 0
				return
			}
			defer response.Body.Close()
			io.Copy(io.Discard, response.Body)
			codes <- response.StatusCode
		}()
	}
	first, second := <-codes, <-codes
	if !((first == 303 && second == 409) || (first == 409 && second == 303)) {
		t.Fatalf("competing starts returned %d and %d", first, second)
	}
	for _, service := range requiredInternalServices {
		decision := "approved"
		if service == "legal" {
			decision = "rejected"
		}
		visa(service, decision)
	}
	status("revision_required")
	var attachmentID int64
	if err := db.QueryRow(ctx, "SELECT id FROM internal_visa_attachments ORDER BY id LIMIT 1").Scan(&attachmentID); err != nil {
		t.Fatal(err)
	}
	attachment, _ := secretary.request("GET", fmt.Sprintf("%s/internal-review/attachments/%d/download", question, attachmentID), nil, nil, 200)
	if len(attachment) < 14<<20 {
		t.Fatal("large attachment was truncated")
	}
	secretary.request("POST", filePath, nil, [][]byte{pdf}, 303)
	secretary.request("POST", question+"/internal-review/restart", url.Values{"deadline": {deadline}, "question_context": {token()}}, nil, 303)
	visa("legal", "approved")
	status("ready_for_committee")
	startVote := func() {
		secretary.request("POST", question+"/committee/start", url.Values{"deadline": {deadline}, "question_context": {token()}}, nil, 303)
	}
	startVote()
	for _, role := range []string{"chair", "member"} {
		users[role].request("POST", question+"/committee/vote", url.Values{"decision": {"against"}, "comment": {"Needs rework"}}, nil, 303)
	}
	status("rejected")
	stale := token()
	users["finance"].request("POST", filePath, nil, [][]byte{pdf}, 303)
	secretary.request("POST", question+"/internal-review/restart", url.Values{"deadline": {deadline}, "question_context": {token()}}, nil, 409)
	secretary.request("POST", filePath+"/3/confirm", nil, nil, 303)
	secretary.request("GET", question, nil, nil, 200)
	secretary.request("POST", question+"/internal-review/restart", url.Values{"deadline": {deadline}, "question_context": {stale}}, nil, 409)
	secretary.request("POST", question+"/internal-review/restart", url.Values{"deadline": {deadline}, "question_context": {token()}}, nil, 303)
	status("ready_for_committee")
	startVote()
	// Test fixture: move only this isolated round's deadline into the past.
	if _, err := db.Exec(ctx, "UPDATE committee_vote_rounds SET deadline=CURRENT_DATE-1 WHERE question_id=$1 AND status='active'", qid); err != nil {
		t.Fatal(err)
	}
	secretary.request("POST", question+"/committee/close", nil, nil, 303)
	status("no_quorum")
	startVote()
	secretary.request("POST", question+"/committee/cancel", nil, nil, 303)
	secretary.request("POST", filePath, nil, [][]byte{pdf}, 303)
	secretary.request("POST", question+"/internal-review/restart", url.Values{"deadline": {deadline}, "question_context": {token()}, "review_service": {revisionVisaKey(fileID, "finance")}}, nil, 303)
	status("internal_review")
	visa("finance", "approved")
	status("ready_for_committee")
	startVote()
	for _, role := range []string{"chair", "member"} {
		users[role].request("POST", question+"/committee/vote", url.Values{"decision": {"for"}}, nil, 303)
	}
	status("approved")
	secretary.request("POST", filePath, nil, [][]byte{pdf}, 409)
	_, protocol := secretary.request("POST", "/protocols", url.Values{"question_id": {strconv.FormatInt(qid, 10)}, fmt.Sprintf("order_%d", qid): {"1"}}, nil, 303)
	secretary.request("GET", protocol, nil, nil, 200)
	secretary.request("GET", protocol+"/word", nil, nil, 200)
	secretary.request("POST", protocol+"/delete", nil, nil, 303)
	secretary.request("POST", "/protocols", url.Values{"question_id": {strconv.FormatInt(qid, 10)}, fmt.Sprintf("order_%d", qid): {"1"}}, nil, 303)
	rows, err := db.Query(ctx, `SELECT id FROM internal_review_rounds WHERE question_id=$1`, qid)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, id := range ids {
		users["chair"].request("GET", fmt.Sprintf("%s/rounds/internal/%d", question, id), nil, nil, 200)
	}
	var before int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM audit_events").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := app.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM audit_events").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("repeated migration changed audit history")
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, auditErr := tx.Exec(ctx, "UPDATE audit_events SET details=details WHERE id=(SELECT MIN(id) FROM audit_events)")
	tx.Rollback(ctx)
	if auditErr == nil {
		t.Fatal("audit update was not rejected")
	}
	admin.request("GET", "/admin", nil, nil, 200)
	admin.request("GET", "/registry", nil, nil, 200)
	users["observer"].request("GET", "/admin", nil, nil, 403)
	users["observer"].request("POST", "/admin/maintenance/preview", url.Values{"action": {"reset"}}, nil, 403)
	_, extra := admin.request("POST", "/questions", url.Values{"question_type": {"budget"}, "title": {"=INTEGRATION-REPORT"}, "decision_text": {"Admin-created question"}, "internal_deadline": {deadline}}, nil, 303)
	admin.request("POST", extra+"/files", url.Values{"title": {"Admin file"}, "category": {"other"}}, [][]byte{pdf}, 303)
	admin.request("POST", extra+"/internal-review/start", url.Values{"deadline": {deadline}}, nil, 303)
	admin.request("POST", extra+"/internal-review/cancel", nil, nil, 303)
	report, _ := admin.request("GET", "/reports/statuses.csv?q=INTEGRATION-REPORT", nil, nil, 200)
	if !strings.Contains(report, "'=INTEGRATION-REPORT") {
		t.Fatal("CSV formula not neutralized")
	}
	hiddenReport, _ := users["chair"].request("GET", "/reports/statuses.csv?q=INTEGRATION-REPORT", nil, nil, 200)
	if strings.Contains(hiddenReport, "=INTEGRATION-REPORT") {
		t.Fatal("report bypassed Committee visibility")
	}
	admin.request("GET", "/reports/statuses?status=approved", nil, nil, 200)
	_, legacy := admin.request("POST", "/documents", url.Values{"title": {"Legacy test"}}, [][]byte{pdf}, 303)
	var legacyID int64
	if err := db.QueryRow(ctx, "SELECT id FROM documents ORDER BY id DESC LIMIT 1").Scan(&legacyID); err != nil {
		t.Fatal(err)
	}
	legacy = fmt.Sprintf("/documents/%d", legacyID)
	preview := func(action, id string) url.Values {
		html, _ := admin.request("POST", "/admin/maintenance/preview", url.Values{"action": {action}, "target_id": {id}}, nil, 200)
		match := regexp.MustCompile(`name="confirmation_token" value="([^"]+)"`).FindStringSubmatch(html)
		if len(match) != 2 {
			t.Fatal("missing maintenance preview token")
		}
		_, phrase, _ := maintenanceAction(action)
		return url.Values{"action": {action}, "target_id": {id}, "confirmation_token": {match[1]}, "confirmation": {phrase}, "password": {password}, "reason": {"Isolated integration test"}}
	}
	individual := preview("archive_question", strings.TrimPrefix(extra, "/questions/"))
	individual.Set("password", "wrong")
	admin.request("POST", "/admin/maintenance/execute", individual, nil, 403)
	individual.Set("password", password)
	admin.request("POST", "/admin/maintenance/execute", individual, nil, 303)
	admin.request("GET", extra, nil, nil, 200)
	admin.request("POST", extra+"/internal-review/start", url.Values{"deadline": {deadline}}, nil, 409)
	archivedReport, _ := admin.request("GET", "/reports/statuses.csv?q=INTEGRATION-REPORT", nil, nil, 200)
	if strings.Contains(archivedReport, "=INTEGRATION-REPORT") {
		t.Fatal("archived material remained in active report")
	}
	admin.request("GET", "/reports/statuses?archive=1", nil, nil, 200)
	admin.request("POST", "/admin/maintenance/execute", preview("archive_document", strings.TrimPrefix(legacy, "/documents/")), nil, 303)
	admin.request("POST", legacy+"/versions", nil, [][]byte{pdf}, 409)
	staleReset := preview("reset", "0")
	admin.request("POST", "/questions", url.Values{"question_type": {"other"}, "title": {"Created after preview"}, "decision_text": {"Test"}, "internal_deadline": {deadline}}, nil, 303)
	admin.request("POST", "/admin/maintenance/execute", staleReset, nil, 409)
	admin.request("POST", "/admin/maintenance/execute", preview("archive_all", "0"), nil, 303)
	users["observer"].request("GET", question, nil, nil, 303)
	var observerID int64
	if err := db.QueryRow(ctx, "SELECT id FROM users WHERE role='observer'").Scan(&observerID); err != nil {
		t.Fatal(err)
	}
	admin.request("POST", fmt.Sprintf("/admin/users/%d/restore", observerID), nil, nil, 303)
	admin.request("POST", fmt.Sprintf("/admin/users/%d/revoke-sessions", observerID), nil, nil, 303)
	// Re-running migrations must also work when archived rows already exist.
	if err := app.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	reset := preview("reset", "0")
	reset.Set("confirmation", "wrong")
	admin.request("POST", "/admin/maintenance/execute", reset, nil, 422)
	reset.Set("confirmation", "УДАЛИТЬ ВСЁ")
	admin.request("POST", "/admin/maintenance/execute", reset, nil, 303)
	var questionsLeft, usersLeft, auditLeft, cleanupLeft int
	if err := db.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM questions),(SELECT COUNT(*) FROM users),(SELECT COUNT(*) FROM audit_events),(SELECT COUNT(*) FROM storage_cleanup WHERE completed_at IS NULL)`).Scan(&questionsLeft, &usersLeft, &auditLeft, &cleanupLeft); err != nil {
		t.Fatal(err)
	}
	if questionsLeft != 0 || usersLeft != 1 || auditLeft != 1 || cleanupLeft == 0 {
		t.Fatalf("reset invariant failed: q=%d users=%d audit=%d cleanup=%d", questionsLeft, usersLeft, auditLeft, cleanupLeft)
	}
	admin.request("GET", "/admin", nil, nil, 200)
	// A failed S3 deletion must remain queued and succeed after recovery.
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer unavailable.Close()
	workingClient := app.storage.client
	failedOptions := workingClient.Options()
	failedOptions.BaseEndpoint = &unavailable.URL
	failedOptions.RetryMaxAttempts = 1
	app.storage.client = s3.New(failedOptions)
	app.processStorageCleanup(ctx)
	app.storage.client = workingClient
	var pendingAfterFailure, attemptsAfterFailure int
	if err := db.QueryRow(ctx, "SELECT COUNT(*),COALESCE(SUM(attempts),0) FROM storage_cleanup WHERE completed_at IS NULL").Scan(&pendingAfterFailure, &attemptsAfterFailure); err != nil {
		t.Fatal(err)
	}
	if pendingAfterFailure != cleanupLeft || attemptsAfterFailure < 1 {
		t.Fatal("failed S3 cleanup lost queued work")
	}
	admin.request("POST", "/admin/storage/retry", nil, nil, 303)
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM storage_cleanup WHERE completed_at IS NULL").Scan(&cleanupLeft); err != nil {
		t.Fatal(err)
	}
	if cleanupLeft != 0 {
		t.Fatal("S3 cleanup did not finish")
	}
	if _, err := app.storage.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &app.storage.bucket, Key: &objectKey}); err == nil {
		t.Fatal("reset object still exists in S3")
	}
	t.Log("Passed real PostgreSQL/S3 workflow, migrations, login/passwords, access checks, immutable downloads, rework, visa carry, Committee cancellation, protocol and history.")
}
