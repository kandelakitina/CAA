package main

import (
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type userListItem struct {
	ID                 int64
	Email              string
	FullName           string
	RoleLabel          string
	Active             bool
	MustChangePassword bool
	IsCurrent          bool
	CreatedLabel       string
}

func (app *application) requireRole(allowed ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		usr := c.MustGet("user").(user)
		for _, role := range allowed {
			if usr.Role == role {
				c.Next()
				return
			}
		}
		c.String(http.StatusForbidden, "Недостаточно прав")
		c.Abort()
	}
}

func (app *application) listUsers(c *gin.Context) ([]userListItem, error) {
	currentUser := c.MustGet("user").(user)
	rows, err := app.db.Query(c.Request.Context(), `
		SELECT id, email, full_name, role, active, must_change_password, created_at
		FROM users
		ORDER BY created_at, id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []userListItem
	for rows.Next() {
		var item userListItem
		var role string
		var created time.Time
		if err := rows.Scan(
			&item.ID, &item.Email, &item.FullName, &role, &item.Active,
			&item.MustChangePassword, &created,
		); err != nil {
			return nil, err
		}
		item.RoleLabel = roleLabel(role)
		item.IsCurrent = item.ID == currentUser.ID
		item.CreatedLabel = created.Format("02.01.2006")
		users = append(users, item)
	}
	return users, rows.Err()
}

func (app *application) renderUsers(c *gin.Context, status int, message string, values gin.H) {
	users, err := app.listUsers(c)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить пользователей")
		return
	}
	if values == nil {
		values = gin.H{}
	}
	if _, exists := values["SelectedRole"]; !exists {
		values["SelectedRole"] = "committee"
	}
	if _, exists := values["ResetUserID"]; !exists {
		values["ResetUserID"] = int64(0)
	}
	if _, exists := values["Success"]; !exists && c.Query("password_reset") == "1" {
		values["Success"] = "Пароль сброшен. Передайте пользователю временный пароль безопасным каналом."
	}
	if _, exists := values["Success"]; !exists && c.Query("user_deleted") == "1" {
		values["Success"] = "Пользователь удалён: вход отключён, действующие сессии завершены."
	}
	values["Title"] = "Пользователи"
	values["User"] = c.MustGet("user").(user)
	values["CSRFToken"] = app.templateCSRF(c)
	values["Users"] = users
	values["Error"] = message
	c.HTML(status, "users.html", values)
}

func (app *application) showUsers(c *gin.Context) {
	app.renderUsers(c, http.StatusOK, "", nil)
}

func (app *application) createUser(c *gin.Context) {
	email := strings.ToLower(strings.TrimSpace(c.PostForm("email")))
	fullName := strings.TrimSpace(c.PostForm("full_name"))
	role := c.PostForm("role")
	password := c.PostForm("temporary_password")
	confirmation := c.PostForm("password_confirmation")
	values := gin.H{"Email": email, "FullName": fullName, "SelectedRole": role}

	parsedAddress, emailError := mail.ParseAddress(email)
	if email == "" || len(email) > 320 || emailError != nil || strings.ToLower(parsedAddress.Address) != email {
		app.renderUsers(c, http.StatusUnprocessableEntity, "Укажите корректный адрес электронной почты", values)
		return
	}
	if fullName == "" || len([]rune(fullName)) > 200 {
		app.renderUsers(c, http.StatusUnprocessableEntity, "Укажите имя длиной до 200 знаков", values)
		return
	}
	if !validAssignableRole(role) {
		app.renderUsers(c, http.StatusUnprocessableEntity, "Выберите допустимую роль", values)
		return
	}
	if len(password) < minimumPasswordLength {
		app.renderUsers(c, http.StatusUnprocessableEntity, "Временный пароль должен содержать не менее 8 символов", values)
		return
	}
	if password != confirmation {
		app.renderUsers(c, http.StatusUnprocessableEntity, "Подтверждение не совпадает с временным паролем", values)
		return
	}
	passwordHash, err := hashPassword(password)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось обработать пароль")
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать создание пользователя")
		return
	}
	defer tx.Rollback(c.Request.Context())
	var userID int64
	err = tx.QueryRow(c.Request.Context(), `
		INSERT INTO users (email, full_name, password_hash, role, must_change_password)
		VALUES ($1, $2, $3, $4, TRUE)
		RETURNING id
	`, email, fullName, passwordHash, role).Scan(&userID)
	if err != nil {
		var databaseError *pgconn.PgError
		if errors.As(err, &databaseError) && databaseError.Code == "23505" {
			app.renderUsers(c, http.StatusConflict, "Пользователь с таким адресом уже существует", values)
			return
		}
		c.String(http.StatusInternalServerError, "Не удалось создать пользователя")
		return
	}
	if err := app.writeAudit(c.Request.Context(), tx, c.MustGet("user").(user), auditRecord{
		EventType: "user.created", TargetType: "user", TargetID: &userID,
		TargetLabel: fullName + " · " + email, Details: roleLabel(role),
	}); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось записать событие аудита")
		return
	}
	if err := tx.Commit(c.Request.Context()); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось создать пользователя")
		return
	}
	c.Redirect(http.StatusSeeOther, "/admin/users")
}

func (app *application) resetUserPassword(c *gin.Context) {
	currentUser := c.MustGet("user").(user)
	userID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || userID < 1 {
		c.String(http.StatusBadRequest, "Некорректный идентификатор пользователя")
		return
	}
	values := gin.H{"ResetUserID": userID}
	if userID == currentUser.ID {
		app.renderUsers(c, http.StatusUnprocessableEntity, "Собственный пароль изменяется через раздел «Пароль»", values)
		return
	}
	password := c.PostForm("temporary_password")
	confirmation := c.PostForm("password_confirmation")
	if len(password) < minimumPasswordLength {
		app.renderUsers(c, http.StatusUnprocessableEntity, "Временный пароль должен содержать не менее 8 символов", values)
		return
	}
	if password != confirmation {
		app.renderUsers(c, http.StatusUnprocessableEntity, "Подтверждение не совпадает с временным паролем", values)
		return
	}
	passwordHash, err := hashPassword(password)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось обработать временный пароль")
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать сброс пароля")
		return
	}
	defer tx.Rollback(c.Request.Context())
	var targetName, targetEmail string
	err = tx.QueryRow(c.Request.Context(), `
		UPDATE users
		SET password_hash = $2, must_change_password = TRUE
		WHERE id = $1 AND active = TRUE
		RETURNING full_name, email
	`, userID, passwordHash).Scan(&targetName, &targetEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		app.renderUsers(c, http.StatusNotFound, "Активный пользователь не найден", values)
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось обновить пароль пользователя")
		return
	}
	if _, err := tx.Exec(c.Request.Context(), `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось завершить действующие сессии пользователя")
		return
	}
	if err := app.writeAudit(c.Request.Context(), tx, currentUser, auditRecord{
		EventType: "user.password_reset", TargetType: "user", TargetID: &userID,
		TargetLabel: targetName + " · " + targetEmail,
	}); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось записать событие аудита")
		return
	}
	if err := tx.Commit(c.Request.Context()); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось сохранить временный пароль")
		return
	}
	c.Redirect(http.StatusSeeOther, "/admin/users?password_reset=1")
}

func (app *application) deleteUser(c *gin.Context) {
	currentUser := c.MustGet("user").(user)
	userID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || userID < 1 {
		c.String(http.StatusBadRequest, "Некорректный идентификатор пользователя")
		return
	}
	if userID == currentUser.ID {
		app.renderUsers(c, http.StatusUnprocessableEntity, "Нельзя удалить собственную учётную запись администратора", nil)
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать удаление пользователя")
		return
	}
	defer tx.Rollback(c.Request.Context())

	var pendingApprovals int
	err = tx.QueryRow(c.Request.Context(), `
		SELECT COUNT(*)
		FROM approval_participants p
		JOIN approval_rounds r ON r.id = p.round_id
		WHERE p.user_id = $1 AND r.status = 'active' AND p.decision IS NULL
	`, userID).Scan(&pendingApprovals)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось проверить активные согласования")
		return
	}
	if pendingApprovals > 0 {
		app.renderUsers(c, http.StatusConflict, "Пользователя нельзя удалить: от него ожидается решение в активном согласовании", nil)
		return
	}
	var targetName, targetEmail string
	err = tx.QueryRow(c.Request.Context(), `
		UPDATE users SET active = FALSE WHERE id = $1 AND active = TRUE
		RETURNING full_name, email
	`, userID).Scan(&targetName, &targetEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		app.renderUsers(c, http.StatusNotFound, "Активный пользователь не найден", nil)
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось удалить пользователя")
		return
	}
	if _, err := tx.Exec(c.Request.Context(), `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось завершить сессии пользователя")
		return
	}
	if err := app.writeAudit(c.Request.Context(), tx, currentUser, auditRecord{
		EventType: "user.deactivated", TargetType: "user", TargetID: &userID,
		TargetLabel: targetName + " · " + targetEmail,
	}); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось записать событие аудита")
		return
	}
	if err := tx.Commit(c.Request.Context()); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось завершить удаление пользователя")
		return
	}
	c.Redirect(http.StatusSeeOther, "/admin/users?user_deleted=1")
}

func (app *application) showChangePassword(c *gin.Context) {
	c.HTML(http.StatusOK, "change-password.html", gin.H{
		"Title":     "Смена пароля",
		"User":      c.MustGet("user").(user),
		"CSRFToken": app.templateCSRF(c),
	})
}

func (app *application) changePassword(c *gin.Context) {
	usr := c.MustGet("user").(user)
	currentPassword := c.PostForm("current_password")
	newPassword := c.PostForm("new_password")
	confirmation := c.PostForm("password_confirmation")

	renderError := func(status int, message string) {
		c.HTML(status, "change-password.html", gin.H{
			"Title":     "Смена пароля",
			"User":      usr,
			"CSRFToken": app.templateCSRF(c),
			"Error":     message,
		})
	}

	var currentHash string
	if err := app.db.QueryRow(c.Request.Context(), `
		SELECT password_hash FROM users WHERE id = $1 AND active = TRUE
	`, usr.ID).Scan(&currentHash); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось проверить текущий пароль")
		return
	}
	if !verifyPassword(currentPassword, currentHash) {
		renderError(http.StatusUnauthorized, "Текущий пароль указан неверно")
		return
	}
	if len(newPassword) < minimumPasswordLength {
		renderError(http.StatusUnprocessableEntity, "Новый пароль должен содержать не менее 8 символов")
		return
	}
	if newPassword != confirmation {
		renderError(http.StatusUnprocessableEntity, "Подтверждение не совпадает с новым паролем")
		return
	}
	if newPassword == currentPassword {
		renderError(http.StatusUnprocessableEntity, "Новый пароль должен отличаться от текущего")
		return
	}

	newHash, err := hashPassword(newPassword)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось обработать новый пароль")
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать смену пароля")
		return
	}
	defer tx.Rollback(c.Request.Context())
	_, err = tx.Exec(c.Request.Context(), `
		UPDATE users
		SET password_hash = $2, must_change_password = FALSE
		WHERE id = $1
	`, usr.ID, newHash)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось сохранить новый пароль")
		return
	}
	if err := app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
		EventType: "user.password_changed", TargetType: "user", TargetID: &usr.ID,
		TargetLabel: usr.FullName + " · " + usr.Email,
	}); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось записать событие аудита")
		return
	}
	if err := tx.Commit(c.Request.Context()); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось сохранить новый пароль")
		return
	}
	c.Redirect(http.StatusSeeOther, "/")
}

func validAssignableRole(role string) bool {
	switch role {
	case "secretary", "committee", "approver", "observer":
		return true
	default:
		return false
	}
}

func roleLabel(role string) string {
	switch role {
	case "admin":
		return "Администратор"
	case "secretary":
		return "Секретарь"
	case "committee":
		return "Член комитета"
	case "approver":
		return "Согласующий"
	case "observer":
		return "Наблюдатель"
	default:
		return role
	}
}
