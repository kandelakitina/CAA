package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

var (
	errQuestionCannotEdit   = errors.New("Редактирование доступно только в черновике до первого запуска согласования")
	errQuestionEditConflict = errors.New("Вопрос изменился после открытия формы. Откройте актуальную форму и перенесите нужные изменения")
)

func canEditQuestion(status string, hasReviewHistory bool) bool {
	return status == "draft" && !hasReviewHistory
}

type questionEditState struct {
	Input     questionInput
	Status    string
	UpdatedAt time.Time
}

type questionEditQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadQuestionEdit(ctx context.Context, db questionEditQueryer, id int64, lock bool) (questionEditState, error) {
	var state questionEditState
	var deadline time.Time
	sql := `SELECT question_type, COALESCE(transaction_subtype, ''), title, summary,
		decision_text, internal_deadline, counterparty, COALESCE(amount::TEXT, ''), currency,
		status, updated_at FROM questions WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	err := db.QueryRow(ctx, sql, id).Scan(&state.Input.QuestionType, &state.Input.TransactionType,
		&state.Input.Title, &state.Input.Summary, &state.Input.DecisionText, &deadline,
		&state.Input.Counterparty, &state.Input.Amount, &state.Input.Currency, &state.Status, &state.UpdatedAt)
	if err != nil {
		return state, err
	}
	state.Input.InternalDeadline = deadline.Format("2006-01-02")
	// Separate query after the row lock: sees rounds committed by a concurrent
	// start/cancel operation before checking whether editing is still allowed.
	var hasHistory bool
	err = db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM internal_review_rounds WHERE question_id = $1)
		OR EXISTS (SELECT 1 FROM committee_vote_rounds WHERE question_id = $1)`, id).Scan(&hasHistory)
	if err != nil {
		return state, err
	}
	if !canEditQuestion(state.Status, hasHistory) {
		return state, errQuestionCannotEdit
	}
	return state, nil
}

func (app *application) renderQuestionEdit(c *gin.Context, status int, id int64, input questionInput, token, message string) {
	c.HTML(status, "question-edit.html", gin.H{
		"Title": "Редактирование черновика", "User": c.MustGet("user").(user),
		"QuestionID": id, "Input": input, "EditToken": token,
		"CSRFToken": app.templateCSRF(c), "Error": message,
	})
}

func (app *application) showQuestionEdit(c *gin.Context) {
	id, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	state, err := loadQuestionEdit(c.Request.Context(), app.db, id, false)
	if errors.Is(err, pgx.ErrNoRows) {
		respondMessage(c, http.StatusNotFound, "Вопрос не найден")
		return
	}
	if errors.Is(err, errQuestionCannotEdit) {
		respondMessage(c, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		respondMessage(c, http.StatusInternalServerError, "Не удалось загрузить черновик")
		return
	}
	app.renderQuestionEdit(c, http.StatusOK, id, state.Input, state.UpdatedAt.Format(time.RFC3339Nano), "")
}

func (app *application) updateQuestion(c *gin.Context) {
	id, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	input := questionInputFromForm(c)
	token := c.PostForm("edit_token")
	expected, err := time.Parse(time.RFC3339Nano, token)
	if err != nil {
		app.renderQuestionEdit(c, http.StatusConflict, id, input, token, errQuestionEditConflict.Error())
		return
	}
	normalized, deadline, err := validateQuestionInput(input, time.Now())
	if err != nil {
		app.renderQuestionEdit(c, http.StatusUnprocessableEntity, id, input, token, err.Error())
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		respondMessage(c, http.StatusInternalServerError, "Не удалось начать сохранение черновика")
		return
	}
	err = app.updateQuestionTransaction(c.Request.Context(), tx, c.MustGet("user").(user), id, normalized, deadline, expected)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		respondMessage(c, http.StatusNotFound, "Вопрос не найден")
	case errors.Is(err, errQuestionCannotEdit), errors.Is(err, errQuestionEditConflict):
		app.renderQuestionEdit(c, http.StatusConflict, id, input, token, err.Error())
	case err != nil:
		app.renderQuestionEdit(c, http.StatusInternalServerError, id, input, token, "Не удалось сохранить черновик")
	default:
		c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", id))
	}
}

func (app *application) updateQuestionTransaction(ctx context.Context, tx pgx.Tx, actor user, id int64, input questionInput, deadline, expected time.Time) error {
	defer tx.Rollback(ctx)
	state, err := loadQuestionEdit(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if !state.UpdatedAt.Equal(expected) {
		return errQuestionEditConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE questions SET question_type = $2, transaction_subtype = NULLIF($3, ''),
		title = $4, summary = $5, decision_text = $6, internal_deadline = $7,
		counterparty = $8, amount = NULLIF($9, '')::NUMERIC, currency = $10, updated_at = clock_timestamp()
		WHERE id = $1`, id, input.QuestionType, input.TransactionType, input.Title, input.Summary,
		input.DecisionText, deadline, input.Counterparty, input.Amount, input.Currency); err != nil {
		return err
	}
	if err := app.writeAudit(ctx, tx, actor, auditRecord{
		EventType: "question.updated", TargetType: "question", TargetID: &id,
		TargetLabel: input.Title, QuestionID: &id, Details: questionEditAuditDetails(state.Input, input),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func questionEditAuditDetails(before, after questionInput) string {
	var changes []string
	for _, field := range []struct{ label, old, new string }{
		{"Тип", before.QuestionType, after.QuestionType}, {"Подтип", before.TransactionType, after.TransactionType},
		{"Название", before.Title, after.Title}, {"Описание", before.Summary, after.Summary},
		{"Формулировка решения", before.DecisionText, after.DecisionText},
		{"Срок", before.InternalDeadline, after.InternalDeadline}, {"Контрагент", before.Counterparty, after.Counterparty},
		{"Сумма", before.Amount, after.Amount}, {"Валюта", before.Currency, after.Currency},
	} {
		if field.old != field.new {
			changes = append(changes, fmt.Sprintf("%s: %q → %q", field.label, field.old, field.new))
		}
	}
	if len(changes) == 0 {
		return "Черновик сохранён без изменения реквизитов"
	}
	return strings.Join(changes, "\n")
}
