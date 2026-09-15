package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

func (app *application) issuePasswordEmail(ctx context.Context, tx pgx.Tx, userID int64, invitation bool) error {
	var passwordHash string
	if err := tx.QueryRow(ctx, `SELECT password_hash FROM users WHERE id=$1 AND active=TRUE FOR UPDATE`, userID).Scan(&passwordHash); err != nil {
		return err
	}
	token, err := randomToken(32)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE email_password_tokens SET used_at=NOW() WHERE user_id=$1 AND used_at IS NULL`, userID); err != nil {
		return err
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO email_password_tokens(user_id,token_hash,password_snapshot,expires_at) VALUES($1,$2,$3,NOW()+INTERVAL '1 hour') RETURNING id`, userID, app.sessionDigest("email-password:"+token), passwordHash).Scan(&id)
	if err != nil {
		return err
	}
	subject := "Сброс пароля — Neva Approvals"
	if invitation {
		subject = "Приглашение — Neva Approvals"
	}
	body := subject + "\n\nЧтобы задать пароль, откройте ссылку в течение одного часа:\n" + app.mail.BaseURL + "/password/reset#" + token + "\n\nСсылка одноразовая. Если вы не ожидали это письмо, не переходите по ссылке."
	return app.queueMail(ctx, tx, userID, subject, body, &id)
}
func (app *application) emailForm(c *gin.Context, status int, forgot bool, message string) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	if app.mail == nil {
		respondMessage(c, http.StatusServiceUnavailable, "Отправка почты пока не настроена. Обратитесь к администратору")
		return
	}
	token, err := randomToken(32)
	if err != nil {
		respondMessage(c, 500, "Не удалось подготовить форму")
		return
	}
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie("email_csrf", token, 3600, "/password", "", true, true)
	linkToken := c.PostForm("token")
	if len(linkToken) != 43 {
		linkToken = ""
	}
	c.HTML(status, "email-password.html", gin.H{"Forgot": forgot, "Message": message, "Token": linkToken, "CSRFToken": app.csrfDigest("email", token)})
}
func (app *application) showEmailPassword(c *gin.Context)  { app.emailForm(c, 200, false, "") }
func (app *application) showForgotPassword(c *gin.Context) { app.emailForm(c, 200, true, "") }
func (app *application) requireEmailCSRF() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Referrer-Policy", "no-referrer")
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
		token, err := c.Cookie("email_csrf")
		if err != nil || !secureTokenEqual(c.PostForm("_csrf"), app.csrfDigest("email", token)) {
			respondMessage(c, 403, "Форма устарела. Обновите страницу")
			c.Abort()
			return
		}
		if app.mail == nil {
			respondMessage(c, 503, "Отправка почты пока не настроена")
			c.Abort()
			return
		}
		c.Next()
	}
}
func (app *application) forgotPassword(c *gin.Context) {
	// Count all requests, including unknown addresses; never reveal account existence.
	key := "email-reset:" + loginLimiterKey(c.Request.RemoteAddr, "")
	if app.loginLimiter != nil {
		if ok, _ := app.loginLimiter.allow(key, time.Now()); !ok {
			app.emailForm(c, 429, true, "Слишком много запросов. Повторите через 15 минут")
			return
		}
		app.loginLimiter.failure(key, time.Now())
	}
	email := strings.ToLower(strings.TrimSpace(c.PostForm("email")))
	if validMailAddress(email) {
		tx, err := app.db.Begin(c.Request.Context())
		if err != nil {
			respondMessage(c, 503, "Не удалось обработать запрос. Повторите позже")
			return
		}
		defer tx.Rollback(c.Request.Context())
		var id int64
		err = tx.QueryRow(c.Request.Context(), `SELECT id FROM users WHERE email=$1 AND active=TRUE FOR UPDATE`, email).Scan(&id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			respondMessage(c, 503, "Не удалось обработать запрос. Повторите позже")
			return
		}
		if err == nil {
			var recent bool
			err = tx.QueryRow(c.Request.Context(), `SELECT EXISTS(SELECT 1 FROM email_password_tokens WHERE user_id=$1 AND created_at>NOW()-INTERVAL '5 minutes' AND used_at IS NULL)`, id).Scan(&recent)
			if err == nil && !recent {
				err = app.issuePasswordEmail(c.Request.Context(), tx, id, false)
			}
			if err != nil {
				respondMessage(c, 503, "Не удалось обработать запрос. Повторите позже")
				return
			}
		}
		if err = tx.Commit(c.Request.Context()); err != nil {
			respondMessage(c, 503, "Не удалось обработать запрос. Повторите позже")
			return
		}
	}
	app.emailForm(c, 200, true, "Если такой активный пользователь существует, письмо со ссылкой будет отправлено. Проверьте входящие и папку «Спам».")
}
func (app *application) consumeEmailPassword(c *gin.Context) {
	token := c.PostForm("token")
	password := c.PostForm("password")
	if len(token) != 43 {
		app.emailForm(c, 422, false, "Ссылка недействительна. Запросите новое письмо")
		return
	}
	if len(password) < minimumPasswordLength || len(password) > 1024 || password != c.PostForm("password_confirmation") {
		app.emailForm(c, 422, false, "Пароль должен содержать не менее 8 символов, подтверждение должно совпадать. Максимальный размер — 1024 байта")
		return
	}
	ctx := c.Request.Context()
	tx, err := app.db.Begin(ctx)
	if err != nil {
		respondMessage(c, 503, "Не удалось сохранить пароль")
		return
	}
	defer tx.Rollback(ctx)
	var usr user
	var tokenID int64
	// Lock the user before tokens, matching invitation/reset/deactivation transactions.
	err = tx.QueryRow(ctx, `SELECT u.id,u.email,u.full_name,u.role FROM users u JOIN email_password_tokens t ON t.user_id=u.id WHERE t.token_hash=$1 AND u.active=TRUE FOR UPDATE OF u`, app.sessionDigest("email-password:"+token)).Scan(&usr.ID, &usr.Email, &usr.FullName, &usr.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		app.emailForm(c, 422, false, "Ссылка недействительна или истекла. Запросите новое письмо")
		return
	}
	if err != nil {
		respondMessage(c, 503, "Не удалось сохранить пароль")
		return
	}
	err = tx.QueryRow(ctx, `UPDATE email_password_tokens t SET used_at=NOW() FROM users u WHERE t.user_id=u.id AND t.token_hash=$1 AND t.used_at IS NULL AND t.expires_at>NOW() AND t.password_snapshot=u.password_hash RETURNING t.id`, app.sessionDigest("email-password:"+token)).Scan(&tokenID)
	if errors.Is(err, pgx.ErrNoRows) {
		app.emailForm(c, 422, false, "Ссылка недействительна или истекла. Запросите новое письмо")
		return
	}
	if err != nil {
		respondMessage(c, 503, "Не удалось сохранить пароль")
		return
	}
	hash, err := hashPassword(password)
	if err != nil {
		respondMessage(c, 500, "Не удалось обработать пароль")
		return
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET password_hash=$2,must_change_password=FALSE WHERE id=$1`, usr.ID, hash); err == nil {
		_, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, usr.ID)
	}
	if err == nil {
		err = app.writeAudit(ctx, tx, usr, auditRecord{EventType: "user.password_changed", TargetType: "user", TargetID: &usr.ID, TargetLabel: usr.FullName + " · " + usr.Email, Details: "Пароль установлен по одноразовой ссылке"})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		respondMessage(c, 503, "Не удалось сохранить пароль")
		return
	}
	app.setSessionCookie(c, "", -1)
	c.Redirect(303, "/login?password_changed=1")
}
