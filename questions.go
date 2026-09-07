package main

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

var (
	amountPattern   = regexp.MustCompile(`^[0-9]{1,18}([.,][0-9]{1,2})?$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
)

type questionListItem struct {
	ID            int64
	Title         string
	TypeLabel     string
	StatusLabel   string
	DeadlineLabel string
	UpdatedLabel  string
}

type questionDetail struct {
	ID                 int64
	Title              string
	TypeLabel          string
	SubtypeLabel       string
	Summary            string
	DecisionText       string
	DeadlineLabel      string
	Counterparty       string
	AmountLabel        string
	Currency           string
	StatusLabel        string
	CreatedBy          string
	CreatedLabel       string
	HasTransactionData bool
}

type questionInput struct {
	QuestionType     string
	TransactionType  string
	Title            string
	Summary          string
	DecisionText     string
	InternalDeadline string
	Counterparty     string
	Amount           string
	Currency         string
}

func (app *application) listQuestions(c *gin.Context, usr user) ([]questionListItem, error) {
	rows, err := app.db.Query(c.Request.Context(), `
		SELECT id, title, question_type, status, internal_deadline, updated_at
		FROM questions
		WHERE $1 <> 'committee' OR status IN ('committee_voting', 'approved', 'rejected', 'no_quorum')
		ORDER BY updated_at DESC, id DESC
	`, usr.Role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []questionListItem
	for rows.Next() {
		var item questionListItem
		var questionType, status string
		var deadline, updated time.Time
		if err := rows.Scan(&item.ID, &item.Title, &questionType, &status, &deadline, &updated); err != nil {
			return nil, err
		}
		item.TypeLabel = questionTypeLabel(questionType)
		item.StatusLabel = questionStatusLabel(status)
		item.DeadlineLabel = deadline.Format("02.01.2006")
		item.UpdatedLabel = updated.Format("02.01.2006 15:04")
		result = append(result, item)
	}
	return result, rows.Err()
}

func questionInputFromForm(c *gin.Context) questionInput {
	return questionInput{
		QuestionType:     strings.TrimSpace(c.PostForm("question_type")),
		TransactionType:  strings.TrimSpace(c.PostForm("transaction_subtype")),
		Title:            strings.TrimSpace(c.PostForm("title")),
		Summary:          strings.TrimSpace(c.PostForm("summary")),
		DecisionText:     strings.TrimSpace(c.PostForm("decision_text")),
		InternalDeadline: strings.TrimSpace(c.PostForm("internal_deadline")),
		Counterparty:     strings.TrimSpace(c.PostForm("counterparty")),
		Amount:           strings.TrimSpace(c.PostForm("amount")),
		Currency:         strings.ToUpper(strings.TrimSpace(c.PostForm("currency"))),
	}
}

func validateQuestionInput(input questionInput, today time.Time) (questionInput, time.Time, error) {
	input.Currency = strings.ToUpper(strings.TrimSpace(input.Currency))
	if !validQuestionType(input.QuestionType) {
		return input, time.Time{}, errors.New("Выберите допустимый тип вопроса")
	}
	if input.Title == "" || len([]rune(input.Title)) > 250 {
		return input, time.Time{}, errors.New("Укажите короткое название длиной до 250 знаков")
	}
	if input.DecisionText == "" || len([]rune(input.DecisionText)) > 10000 {
		return input, time.Time{}, errors.New("Укажите формулировку решения длиной до 10 000 знаков")
	}
	if len([]rune(input.Summary)) > 5000 {
		return input, time.Time{}, errors.New("Краткое описание не должно превышать 5 000 знаков")
	}
	deadline, err := time.Parse("2006-01-02", input.InternalDeadline)
	if err != nil {
		return input, time.Time{}, errors.New("Укажите срок внутреннего согласования")
	}
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())
	if deadline.Before(today) {
		return input, time.Time{}, errors.New("Срок внутреннего согласования не может быть в прошлом")
	}
	if input.QuestionType != "transaction" {
		input.TransactionType = ""
		input.Counterparty = ""
		input.Amount = ""
		input.Currency = ""
		return input, deadline, nil
	}
	if input.TransactionType != "purchase" && input.TransactionType != "financial" {
		return input, time.Time{}, errors.New("Выберите подтип сделки или договора")
	}
	if input.Counterparty == "" || len([]rune(input.Counterparty)) > 500 {
		return input, time.Time{}, errors.New("Укажите контрагента длиной до 500 знаков")
	}
	if !amountPattern.MatchString(input.Amount) {
		return input, time.Time{}, errors.New("Укажите положительную сумму не более чем с двумя знаками после запятой")
	}
	input.Amount = strings.Replace(input.Amount, ",", ".", 1)
	if strings.Trim(strings.ReplaceAll(input.Amount, ".", ""), "0") == "" {
		return input, time.Time{}, errors.New("Сумма должна быть больше нуля")
	}
	if !currencyPattern.MatchString(input.Currency) {
		return input, time.Time{}, errors.New("Укажите трёхбуквенный код валюты, например RUB")
	}
	return input, deadline, nil
}

func (app *application) createQuestion(c *gin.Context) {
	usr := c.MustGet("user").(user)
	input, deadline, err := validateQuestionInput(questionInputFromForm(c), time.Now())
	if err != nil {
		app.renderDashboardWithQuestion(c, http.StatusUnprocessableEntity, err.Error(), input)
		return
	}

	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать создание вопроса")
		return
	}
	defer tx.Rollback(c.Request.Context())

	var questionID int64
	err = tx.QueryRow(c.Request.Context(), `
		INSERT INTO questions (
			question_type, transaction_subtype, title, summary, decision_text,
			internal_deadline, counterparty, amount, currency, created_by
		) VALUES (
			$1, NULLIF($2, ''), $3, $4, $5, $6,
			$7, NULLIF($8, '')::NUMERIC, $9, $10
		)
		RETURNING id
	`, input.QuestionType, input.TransactionType, input.Title, input.Summary,
		input.DecisionText, deadline, input.Counterparty, input.Amount, input.Currency, usr.ID).Scan(&questionID)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось создать вопрос")
		return
	}
	if err := app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
		EventType: "question.created", TargetType: "question", TargetID: &questionID,
		TargetLabel: input.Title, QuestionID: &questionID, Details: questionTypeLabel(input.QuestionType),
	}); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось записать событие аудита")
		return
	}
	if err := tx.Commit(c.Request.Context()); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось сохранить вопрос")
		return
	}
	c.Redirect(http.StatusSeeOther, "/questions/"+strconv.FormatInt(questionID, 10))
}

func (app *application) showQuestion(c *gin.Context) {
	usr := c.MustGet("user").(user)
	questionID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || questionID < 1 {
		c.String(http.StatusBadRequest, "Некорректный идентификатор вопроса")
		return
	}

	var detail questionDetail
	var questionType, subtype, status, amount string
	var deadline, createdAt time.Time
	err = app.db.QueryRow(c.Request.Context(), `
		SELECT q.id, q.title, q.question_type, COALESCE(q.transaction_subtype, ''),
		       q.summary, q.decision_text, q.internal_deadline, q.counterparty,
		       COALESCE(q.amount::TEXT, ''), q.currency, q.status,
		       u.full_name, q.created_at
		FROM questions q
		JOIN users u ON u.id = q.created_by
		WHERE q.id = $1
		  AND ($2 <> 'committee' OR q.status IN ('committee_voting', 'approved', 'rejected', 'no_quorum'))
	`, questionID, usr.Role).Scan(
		&detail.ID, &detail.Title, &questionType, &subtype, &detail.Summary,
		&detail.DecisionText, &deadline, &detail.Counterparty,
		&amount, &detail.Currency, &status, &detail.CreatedBy, &createdAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Вопрос не найден")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить вопрос")
		return
	}
	detail.DeadlineLabel = deadline.Format("02.01.2006")
	detail.TypeLabel = questionTypeLabel(questionType)
	detail.SubtypeLabel = transactionSubtypeLabel(subtype)
	detail.StatusLabel = questionStatusLabel(status)
	detail.AmountLabel = strings.TrimRight(strings.TrimRight(amount, "0"), ".")
	detail.CreatedLabel = createdAt.Format("02.01.2006 15:04")
	detail.HasTransactionData = questionType == "transaction"

	c.HTML(http.StatusOK, "question.html", gin.H{
		"Title": detail.Title, "User": usr, "CSRFToken": app.templateCSRF(c), "Question": detail,
	})
}

func validQuestionType(value string) bool {
	switch value {
	case "budget", "internal_document", "transaction", "kpi", "board_other", "organizational", "other":
		return true
	default:
		return false
	}
}

func questionTypeLabel(value string) string {
	labels := map[string]string{
		"budget": "Бюджет", "internal_document": "Внутренний документ / ЛНА",
		"transaction": "Сделка / договор", "kpi": "КПЭ",
		"board_other":    "Иной вопрос компетенции Совета директоров",
		"organizational": "Организационный вопрос", "other": "Другое",
	}
	return labels[value]
}

func transactionSubtypeLabel(value string) string {
	if value == "purchase" {
		return "Закупочный"
	}
	if value == "financial" {
		return "Финансовый"
	}
	return ""
}

func questionStatusLabel(value string) string {
	labels := map[string]string{
		"draft": "Черновик", "internal_review": "Внутреннее согласование",
		"revision_required": "Требуется доработка", "ready_for_committee": "Готов к передаче",
		"committee_voting": "Голосование Комитета", "approved": "Решение принято",
		"rejected": "Решение не принято", "no_quorum": "Нет кворума", "cancelled": "Отменён",
	}
	return labels[value]
}
