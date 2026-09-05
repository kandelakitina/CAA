package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	passwordIterations    = 210_000
	minimumPasswordLength = 8
	sessionLifetime       = 12 * time.Hour
)

type application struct {
	db            *pgxpool.Pool
	sessionSecret []byte
	storage       *storage
	loginLimiter  *loginLimiter
}

type user struct {
	ID                 int64
	Email              string
	FullName           string
	Role               string
	MustChangePassword bool
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	db, err := pgxpool.New(ctx, "")
	if err != nil {
		log.Fatalf("configure database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(ctx); err != nil {
		log.Fatalf("connect to database: %v", err)
	}

	secret := os.Getenv("SESSION_SECRET")
	if len(secret) < 32 {
		log.Fatal("SESSION_SECRET must contain at least 32 characters")
	}

	app := &application{
		db:            db,
		sessionSecret: []byte(secret),
		loginLimiter:  newLoginLimiter(5, 15*time.Minute),
	}
	app.storage, err = newStorage(ctx)
	if err != nil {
		log.Printf("S3 storage is not ready: %v", err)
	}
	if err := app.migrate(ctx); err != nil {
		log.Fatalf("run database migration: %v", err)
	}
	if err := app.createInitialAdmin(ctx); err != nil {
		log.Fatalf("create initial administrator: %v", err)
	}

	router := gin.Default()
	router.LoadHTMLGlob("templates/*")
	router.Static("/static", "./static")

	router.GET("/health", app.health)
	router.GET("/login", app.showLogin)
	router.POST("/login", app.requireLoginCSRF(), app.login)
	router.POST("/logout", app.requireUser(), app.requireCSRF(), app.logout)
	router.GET("/password/change", app.requireUser(), app.showChangePassword)
	router.POST("/password/change", app.requireUser(), app.requireCSRF(), app.changePassword)
	router.GET("/admin/users", app.requireUser(), app.requireRole("admin"), app.showUsers)
	router.GET("/admin/audit", app.requireUser(), app.requireRole("admin"), app.showAuditLog)
	router.POST("/admin/users", app.requireUser(), app.requireCSRF(), app.requireRole("admin"), app.createUser)
	router.POST("/admin/users/:id/reset-password", app.requireUser(), app.requireCSRF(), app.requireRole("admin"), app.resetUserPassword)
	router.POST("/admin/users/:id/delete", app.requireUser(), app.requireCSRF(), app.requireRole("admin"), app.deleteUser)
	router.GET("/", app.requireUser(), app.dashboard)
	router.GET("/storage/check", app.requireUser(), app.checkStorage)
	router.POST("/documents", app.requireUser(), app.requireCSRF(), app.createDocument)
	router.GET("/documents/:id", app.requireUser(), app.showDocument)
	router.POST("/documents/:id/versions", app.requireUser(), app.requireCSRF(), app.createDocumentVersion)
	router.GET("/documents/:id/download", app.requireUser(), app.downloadCurrentDocument)
	router.GET("/documents/:id/versions/:version/download", app.requireUser(), app.downloadDocumentVersion)
	router.POST("/documents/:id/approval/start", app.requireUser(), app.requireCSRF(), app.requireRole("admin", "secretary"), app.startApproval)
	router.POST("/documents/:id/approval/respond", app.requireUser(), app.requireCSRF(), app.respondToApproval)
	router.POST("/documents/:id/approval/complete", app.requireUser(), app.requireCSRF(), app.requireRole("admin", "secretary"), app.completeApproval)
	router.POST("/documents/:id/approval/cancel", app.requireUser(), app.requireCSRF(), app.requireRole("admin", "secretary"), app.cancelApproval)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	address := "0.0.0.0:" + port
	log.Printf("starting server on %s", address)
	if err := router.Run(address); err != nil {
		log.Fatal(err)
	}
}

func (app *application) migrate(ctx context.Context) error {
	const schema = `
		CREATE TABLE IF NOT EXISTS users (
			id BIGSERIAL PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			full_name TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL CHECK (role IN ('admin', 'secretary', 'reviewer', 'observer')),
			active BOOLEAN NOT NULL DEFAULT TRUE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		ALTER TABLE users ADD COLUMN IF NOT EXISTS must_change_password BOOLEAN NOT NULL DEFAULT FALSE;
		ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
		UPDATE users SET role = 'approver' WHERE role = 'reviewer';
		ALTER TABLE users ADD CONSTRAINT users_role_check
			CHECK (role IN ('admin', 'secretary', 'committee', 'approver', 'observer'));

		CREATE TABLE IF NOT EXISTS sessions (
			token_hash TEXT PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE INDEX IF NOT EXISTS sessions_user_id_idx ON sessions(user_id);
		CREATE INDEX IF NOT EXISTS sessions_expires_at_idx ON sessions(expires_at);

		CREATE TABLE IF NOT EXISTS documents (
			id BIGSERIAL PRIMARY KEY,
			title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'in_review', 'approved', 'rejected')),
			current_version INTEGER NOT NULL DEFAULT 0,
			created_by BIGINT NOT NULL REFERENCES users(id),
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS document_versions (
			id BIGSERIAL PRIMARY KEY,
			document_id BIGINT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
			version_no INTEGER NOT NULL CHECK (version_no > 0),
			object_key TEXT NOT NULL UNIQUE,
			s3_version_id TEXT,
			original_filename TEXT NOT NULL,
			content_type TEXT NOT NULL,
			size_bytes BIGINT NOT NULL CHECK (size_bytes > 0),
			uploaded_by BIGINT NOT NULL REFERENCES users(id),
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (document_id, version_no)
		);

		CREATE INDEX IF NOT EXISTS documents_updated_at_idx ON documents(updated_at DESC);
		CREATE INDEX IF NOT EXISTS document_versions_document_id_idx ON document_versions(document_id, version_no DESC);

		CREATE TABLE IF NOT EXISTS approval_rounds (
			id BIGSERIAL PRIMARY KEY,
			document_id BIGINT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
			version_no INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'completed', 'cancelled')),
			started_by BIGINT NOT NULL REFERENCES users(id),
			deadline DATE,
			final_outcome TEXT CHECK (final_outcome IS NULL OR final_outcome IN ('approved', 'rejected')),
			final_comment TEXT NOT NULL DEFAULT '',
			started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			completed_at TIMESTAMPTZ,
			FOREIGN KEY (document_id, version_no) REFERENCES document_versions(document_id, version_no)
		);

		CREATE UNIQUE INDEX IF NOT EXISTS approval_rounds_one_active_idx
			ON approval_rounds(document_id) WHERE status = 'active';
		CREATE INDEX IF NOT EXISTS approval_rounds_document_idx
			ON approval_rounds(document_id, started_at DESC);

		CREATE TABLE IF NOT EXISTS approval_participants (
			id BIGSERIAL PRIMARY KEY,
			round_id BIGINT NOT NULL REFERENCES approval_rounds(id) ON DELETE CASCADE,
			user_id BIGINT NOT NULL REFERENCES users(id),
			role_snapshot TEXT NOT NULL CHECK (role_snapshot IN ('committee', 'approver')),
			decision TEXT CHECK (decision IS NULL OR decision IN ('approve', 'approve_with_comments', 'reject', 'abstain')),
			comment TEXT NOT NULL DEFAULT '',
			responded_at TIMESTAMPTZ,
			UNIQUE (round_id, user_id)
		);

		CREATE INDEX IF NOT EXISTS approval_participants_user_idx
			ON approval_participants(user_id, round_id);

		CREATE TABLE IF NOT EXISTS audit_events (
			id BIGSERIAL PRIMARY KEY,
			actor_user_id BIGINT NOT NULL,
			actor_name TEXT NOT NULL,
			actor_email TEXT NOT NULL,
			actor_role TEXT NOT NULL,
			event_type TEXT NOT NULL,
			target_type TEXT NOT NULL DEFAULT '',
			target_id BIGINT,
			target_label TEXT NOT NULL DEFAULT '',
			document_id BIGINT,
			version_no INTEGER,
			details TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE INDEX IF NOT EXISTS audit_events_created_at_idx
			ON audit_events(created_at DESC, id DESC);
		CREATE INDEX IF NOT EXISTS audit_events_actor_idx
			ON audit_events(actor_user_id, created_at DESC);
		CREATE INDEX IF NOT EXISTS audit_events_document_idx
			ON audit_events(document_id, created_at DESC) WHERE document_id IS NOT NULL;

		CREATE OR REPLACE FUNCTION prevent_audit_event_changes()
		RETURNS TRIGGER AS $$
		BEGIN
			RAISE EXCEPTION 'audit events are immutable';
		END;
		$$ LANGUAGE plpgsql;

		DO $$
		BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM pg_trigger
				WHERE tgname = 'audit_events_immutable_trigger'
				  AND tgrelid = 'audit_events'::regclass
			) THEN
				CREATE TRIGGER audit_events_immutable_trigger
				BEFORE UPDATE OR DELETE ON audit_events
				FOR EACH ROW EXECUTE FUNCTION prevent_audit_event_changes();
			END IF;
		END;
		$$;
	`
	_, err := app.db.Exec(ctx, schema)
	return err
}

func (app *application) createInitialAdmin(ctx context.Context) error {
	email := strings.ToLower(strings.TrimSpace(os.Getenv("INITIAL_ADMIN_EMAIL")))
	password := os.Getenv("INITIAL_ADMIN_PASSWORD")
	name := strings.TrimSpace(os.Getenv("INITIAL_ADMIN_NAME"))

	var count int
	if err := app.db.QueryRow(ctx, "SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	if email == "" || password == "" {
		return errors.New("database has no users: set INITIAL_ADMIN_EMAIL and INITIAL_ADMIN_PASSWORD")
	}
	if len(password) < minimumPasswordLength {
		return errors.New("INITIAL_ADMIN_PASSWORD must contain at least 8 characters")
	}
	if name == "" {
		name = "Администратор"
	}

	passwordHash, err := hashPassword(password)
	if err != nil {
		return err
	}
	_, err = app.db.Exec(ctx, `
		INSERT INTO users (email, full_name, password_hash, role)
		VALUES ($1, $2, $3, 'admin')
		ON CONFLICT (email) DO NOTHING
	`, email, name, passwordHash)
	if err == nil {
		log.Printf("initial administrator created for %s", email)
	}
	return err
}

func (app *application) health(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	if err := app.db.Ping(ctx); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "database unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (app *application) showLogin(c *gin.Context) {
	if _, ok := app.currentUser(c); ok {
		c.Redirect(http.StatusSeeOther, "/")
		return
	}
	token, err := app.issueLoginCSRF(c)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось подготовить форму входа")
		return
	}
	c.HTML(http.StatusOK, "login.html", gin.H{"CSRFToken": token})
}

func (app *application) login(c *gin.Context) {
	email := strings.ToLower(strings.TrimSpace(c.PostForm("email")))
	password := c.PostForm("password")
	limiterKey := loginLimiterKey(c.Request.RemoteAddr, email)
	if allowed, retryAfter := app.loginLimiter.allow(limiterKey, time.Now()); !allowed {
		c.Header("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		c.HTML(http.StatusTooManyRequests, "login.html", gin.H{
			"Email":     email,
			"CSRFToken": c.PostForm("_csrf"),
			"Error":     "Слишком много неудачных попыток. Повторите вход позже",
		})
		return
	}

	var usr user
	var passwordHash string
	err := app.db.QueryRow(c.Request.Context(), `
		SELECT id, email, full_name, role, must_change_password, COALESCE(password_hash, '')
		FROM users
		WHERE email = $1 AND active = TRUE
	`, email).Scan(&usr.ID, &usr.Email, &usr.FullName, &usr.Role, &usr.MustChangePassword, &passwordHash)
	if err != nil || !verifyPassword(password, passwordHash) {
		app.loginLimiter.failure(limiterKey, time.Now())
		c.HTML(http.StatusUnauthorized, "login.html", gin.H{
			"Email":     email,
			"CSRFToken": c.PostForm("_csrf"),
			"Error":     "Неверный адрес электронной почты или пароль",
		})
		return
	}
	app.loginLimiter.success(limiterKey)

	token, err := randomToken(32)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось создать сессию")
		return
	}
	expiresAt := time.Now().Add(sessionLifetime)
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать создание сессии")
		return
	}
	defer tx.Rollback(c.Request.Context())
	_, err = tx.Exec(c.Request.Context(), `
		INSERT INTO sessions (token_hash, user_id, expires_at)
		VALUES ($1, $2, $3)
	`, app.sessionDigest(token), usr.ID, expiresAt)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось сохранить сессию")
		return
	}
	if err := app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
		EventType: "session.login", TargetType: "user", TargetID: &usr.ID,
		TargetLabel: usr.FullName + " · " + usr.Email,
	}); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось записать событие аудита")
		return
	}
	if err := tx.Commit(c.Request.Context()); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось сохранить сессию")
		return
	}

	app.setSessionCookie(c, token, int(sessionLifetime.Seconds()))
	if usr.MustChangePassword {
		c.Redirect(http.StatusSeeOther, "/password/change")
		return
	}
	c.Redirect(http.StatusSeeOther, "/")
}

func (app *application) logout(c *gin.Context) {
	usr := c.MustGet("user").(user)
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось завершить сессию")
		return
	}
	defer tx.Rollback(c.Request.Context())
	if token, err := c.Cookie("session_token"); err == nil {
		if _, err := tx.Exec(c.Request.Context(), "DELETE FROM sessions WHERE token_hash = $1", app.sessionDigest(token)); err != nil {
			c.String(http.StatusInternalServerError, "Не удалось завершить сессию")
			return
		}
	}
	if err := app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
		EventType: "session.logout", TargetType: "user", TargetID: &usr.ID,
		TargetLabel: usr.FullName + " · " + usr.Email,
	}); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось записать событие аудита")
		return
	}
	if err := tx.Commit(c.Request.Context()); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось завершить сессию")
		return
	}
	app.setSessionCookie(c, "", -1)
	c.Redirect(http.StatusSeeOther, "/login")
}

func (app *application) dashboard(c *gin.Context) {
	app.renderDashboard(c, http.StatusOK, "")
}

func (app *application) requireUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		usr, ok := app.currentUser(c)
		if !ok {
			c.Redirect(http.StatusSeeOther, "/login")
			c.Abort()
			return
		}
		c.Set("user", usr)
		if usr.MustChangePassword && c.Request.URL.Path != "/password/change" && c.Request.URL.Path != "/logout" {
			c.Redirect(http.StatusSeeOther, "/password/change")
			c.Abort()
			return
		}
		c.Next()
	}
}

func (app *application) currentUser(c *gin.Context) (user, bool) {
	token, err := c.Cookie("session_token")
	if err != nil || token == "" {
		return user{}, false
	}

	var usr user
	err = app.db.QueryRow(c.Request.Context(), `
		SELECT u.id, u.email, u.full_name, u.role, u.must_change_password
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.expires_at > NOW()
		  AND u.active = TRUE
	`, app.sessionDigest(token)).Scan(&usr.ID, &usr.Email, &usr.FullName, &usr.Role, &usr.MustChangePassword)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("read session: %v", err)
		}
		return user{}, false
	}
	return usr, true
}

func (app *application) setSessionCookie(c *gin.Context, token string, maxAge int) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("session_token", token, maxAge, "/", "", true, true)
}

func (app *application) sessionDigest(token string) string {
	mac := hmac.New(sha256.New, app.sessionSecret)
	_, _ = mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	derived := pbkdf2Key([]byte(password), salt, passwordIterations, 32, sha256.New)
	return fmt.Sprintf("pbkdf2_sha256$%d$%s$%s",
		passwordIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(derived),
	), nil
}

func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 1 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	actual := pbkdf2Key([]byte(password), salt, iterations, len(expected), sha256.New)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func pbkdf2Key(password, salt []byte, iterations, keyLength int, newHash func() hash.Hash) []byte {
	hashLength := newHash().Size()
	blocks := (keyLength + hashLength - 1) / hashLength
	result := make([]byte, 0, blocks*hashLength)

	for block := 1; block <= blocks; block++ {
		mac := hmac.New(newHash, password)
		_, _ = mac.Write(salt)
		var counter [4]byte
		binary.BigEndian.PutUint32(counter[:], uint32(block))
		_, _ = mac.Write(counter[:])
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)

		for i := 1; i < iterations; i++ {
			mac = hmac.New(newHash, password)
			_, _ = mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		result = append(result, t...)
	}
	return result[:keyLength]
}

func randomToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
