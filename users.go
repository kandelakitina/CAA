package main

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
)

type userListItem struct {
	ID                 int64
	Email              string
	FullName           string
	RoleLabel          string
	Active             bool
	MustChangePassword bool
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
	values["Title"] = "Пользователи"
	values["User"] = c.MustGet("user").(user)
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
	values := gin.H{"Email": email, "FullName": fullName, "SelectedRole": role}

	if email == "" || len(email) > 320 || !strings.Contains(email, "@") {
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
	if len(password) < 12 {
		app.renderUsers(c, http.StatusUnprocessableEntity, "Временный пароль должен содержать не менее 12 символов", values)
		return
	}

	passwordHash, err := hashPassword(password)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось обработать пароль")
		return
	}
	_, err = app.db.Exec(c.Request.Context(), `
		INSERT INTO users (email, full_name, password_hash, role, must_change_password)
		VALUES ($1, $2, $3, $4, TRUE)
	`, email, fullName, passwordHash, role)
	if err != nil {
		var databaseError *pgconn.PgError
		if errors.As(err, &databaseError) && databaseError.Code == "23505" {
			app.renderUsers(c, http.StatusConflict, "Пользователь с таким адресом уже существует", values)
			return
		}
		c.String(http.StatusInternalServerError, "Не удалось создать пользователя")
		return
	}
	c.Redirect(http.StatusSeeOther, "/admin/users")
}

func (app *application) showChangePassword(c *gin.Context) {
	c.HTML(http.StatusOK, "change-password.html", gin.H{
		"Title": "Смена пароля",
		"User":  c.MustGet("user").(user),
	})
}

func (app *application) changePassword(c *gin.Context) {
	usr := c.MustGet("user").(user)
	currentPassword := c.PostForm("current_password")
	newPassword := c.PostForm("new_password")
	confirmation := c.PostForm("password_confirmation")

	renderError := func(status int, message string) {
		c.HTML(status, "change-password.html", gin.H{
			"Title": "Смена пароля",
			"User":  usr,
			"Error": message,
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
	if len(newPassword) < 12 {
		renderError(http.StatusUnprocessableEntity, "Новый пароль должен содержать не менее 12 символов")
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
	_, err = app.db.Exec(c.Request.Context(), `
		UPDATE users
		SET password_hash = $2, must_change_password = FALSE
		WHERE id = $1
	`, usr.ID, newHash)
	if err != nil {
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
