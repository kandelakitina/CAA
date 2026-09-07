package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

var requiredInternalServices = []string{"legal", "finance", "construction", "security"}

type reviewStartFile struct {
	ID        int64
	VersionNo int
	Category  string
}

type internalReviewView struct {
	HasRound      bool
	Active        bool
	CanStart      bool
	CanManage     bool
	StatusLabel   string
	DeadlineLabel string
	ProgressLabel string
	PastDue       bool
	Items         []internalReviewItem
}

type internalReviewItem struct {
	RequirementID int64
	FileTitle     string
	VersionNo     int
	Service       string
	ServiceLabel  string
	FilePaused    bool
	CanRespond    bool
	HasVisa       bool
	Visa          internalVisaItem
	History       []internalVisaItem
}

type internalVisaItem struct {
	ID             int64
	DecisionLabel  string
	Comment        string
	DecidedBy      string
	DecidedLabel   string
	Withdrawn      bool
	WithdrawnLabel string
	CanWithdraw    bool
}

func (app *application) requireInternalApprover() gin.HandlerFunc {
	return func(c *gin.Context) {
		usr := c.MustGet("user").(user)
		if usr.Role != "approver" || !validInternalService(usr.InternalService) {
			c.String(http.StatusForbidden, "Решение может дать только назначенный внутренний согласующий")
			c.Abort()
			return
		}
		c.Next()
	}
}

func validInternalDecision(value string) bool {
	switch value {
	case "approved", "approved_with_comments", "rejected", "no_comments_without_review":
		return true
	default:
		return false
	}
}

func validateInternalDecision(decision, comment string) error {
	if !validInternalDecision(decision) {
		return errors.New("Выберите допустимое решение")
	}
	if len([]rune(comment)) > 5000 {
		return errors.New("Комментарий не должен превышать 5 000 знаков")
	}
	if (decision == "approved_with_comments" || decision == "rejected") && comment == "" {
		return errors.New("Для выбранного решения укажите комментарий")
	}
	return nil
}

func internalDecisionLabel(value string) string {
	labels := map[string]string{
		"approved":                   "Согласовано",
		"approved_with_comments":     "Согласовано с замечаниями",
		"rejected":                   "Не согласовано",
		"no_comments_without_review": "Нет замечаний (без проверки)",
	}
	return labels[value]
}

func validateInternalReviewStart(questionType string, deadline, today time.Time, files []reviewStartFile, activeServices map[string]int, pendingUploads int) error {
	day := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())
	if deadline.Before(day) {
		return errors.New("Срок внутреннего согласования уже прошёл")
	}
	if pendingUploads > 0 {
		return errors.New("Сначала подтвердите или отклоните все ожидающие загрузки")
	}
	if len(files) == 0 {
		return errors.New("Добавьте хотя бы один официальный файл")
	}
	categories := make(map[string]bool, len(files))
	for _, file := range files {
		categories[file.Category] = true
	}
	if questionType == "transaction" && (!categories["contract"] || !categories["terms_summary"]) {
		return errors.New("Для договора нужны основной договор и справка об основных условиях")
	}
	if questionType == "internal_document" && !categories["lna_draft"] {
		return errors.New("Для внутреннего документа добавьте проект ЛНА")
	}
	for _, service := range requiredInternalServices {
		if activeServices[service] < 1 {
			return fmt.Errorf("Нет активного представителя: %s", internalServiceLabel(service))
		}
	}
	return nil
}

func (app *application) startInternalReview(c *gin.Context) {
	usr := c.MustGet("user").(user)
	questionID, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	requestedDeadline, err := time.Parse("2006-01-02", strings.TrimSpace(c.PostForm("deadline")))
	if err != nil {
		c.String(http.StatusUnprocessableEntity, "Укажите срок внутреннего согласования")
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать внутреннее согласование")
		return
	}
	defer tx.Rollback(c.Request.Context())
	if err := lockUserAssignments(c, tx); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось проверить назначения пользователей")
		return
	}

	var title, questionType, status string
	err = tx.QueryRow(c.Request.Context(), `
		SELECT title, question_type, status
		FROM questions WHERE id = $1 FOR UPDATE
	`, questionID).Scan(&title, &questionType, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Вопрос не найден")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить вопрос")
		return
	}
	if status != "draft" {
		c.String(http.StatusConflict, "Внутреннее согласование можно запустить только из черновика")
		return
	}
	rows, err := tx.Query(c.Request.Context(), `
		SELECT id, current_version_no, category
		FROM question_files
		WHERE question_id = $1 AND status = 'active' AND current_version_no IS NOT NULL
		ORDER BY id FOR UPDATE
	`, questionID)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось проверить комплект файлов")
		return
	}
	var files []reviewStartFile
	for rows.Next() {
		var file reviewStartFile
		if err := rows.Scan(&file.ID, &file.VersionNo, &file.Category); err != nil {
			rows.Close()
			c.String(http.StatusInternalServerError, "Не удалось прочитать комплект файлов")
			return
		}
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		c.String(http.StatusInternalServerError, "Не удалось прочитать комплект файлов")
		return
	}
	rows.Close()
	var pendingUploads int
	if err := tx.QueryRow(c.Request.Context(), `
		SELECT COUNT(*) FROM question_file_versions v
		JOIN question_files f ON f.id = v.question_file_id
		WHERE f.question_id = $1 AND f.status = 'active' AND v.approval_status = 'pending'
	`, questionID).Scan(&pendingUploads); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось проверить ожидающие загрузки")
		return
	}
	serviceRows, err := tx.Query(c.Request.Context(), `
		SELECT internal_service, COUNT(*) FROM users
		WHERE active = TRUE AND role = 'approver' AND internal_service IS NOT NULL
		GROUP BY internal_service
	`)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось проверить представителей дирекций")
		return
	}
	activeServices := make(map[string]int)
	for serviceRows.Next() {
		var service string
		var count int
		if err := serviceRows.Scan(&service, &count); err != nil {
			serviceRows.Close()
			c.String(http.StatusInternalServerError, "Не удалось прочитать представителей дирекций")
			return
		}
		activeServices[service] = count
	}
	if err := serviceRows.Err(); err != nil {
		serviceRows.Close()
		c.String(http.StatusInternalServerError, "Не удалось прочитать представителей дирекций")
		return
	}
	serviceRows.Close()
	if err := validateInternalReviewStart(questionType, requestedDeadline, time.Now(), files, activeServices, pendingUploads); err != nil {
		c.String(http.StatusUnprocessableEntity, err.Error())
		return
	}
	deadline := requestedDeadline
	var roundID int64
	err = tx.QueryRow(c.Request.Context(), `
		INSERT INTO internal_review_rounds (question_id, deadline, started_by)
		VALUES ($1, $2, $3) RETURNING id
	`, questionID, deadline, usr.ID).Scan(&roundID)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось создать раунд внутреннего согласования")
		return
	}
	for _, file := range files {
		for _, service := range requiredInternalServices {
			_, err = tx.Exec(c.Request.Context(), `
				INSERT INTO internal_review_requirements (round_id, question_file_id, version_no, internal_service)
				VALUES ($1, $2, $3, $4)
			`, roundID, file.ID, file.VersionNo, service)
			if err != nil {
				c.String(http.StatusInternalServerError, "Не удалось сформировать задания дирекциям")
				return
			}
		}
	}
	_, err = tx.Exec(c.Request.Context(), `UPDATE questions SET status = 'internal_review', internal_deadline = $2, updated_at = NOW() WHERE id = $1`, questionID, deadline)
	if err == nil {
		err = app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
			EventType: "internal_review.started", TargetType: "question", TargetID: &questionID,
			TargetLabel: title, QuestionID: &questionID,
			Details: fmt.Sprintf("Раунд №%d, файлов: %d, срок: %s", roundID, len(files), deadline.Format("02.01.2006")),
		})
	}
	if err == nil {
		err = tx.Commit(c.Request.Context())
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось запустить внутреннее согласование")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", questionID))
}

func (app *application) respondInternalReview(c *gin.Context) {
	usr := c.MustGet("user").(user)
	questionID, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	requirementID, err := strconv.ParseInt(strings.TrimSpace(c.PostForm("requirement_id")), 10, 64)
	if err != nil || requirementID < 1 {
		c.String(http.StatusBadRequest, "Некорректное задание на согласование")
		return
	}
	decision := strings.TrimSpace(c.PostForm("decision"))
	comment := strings.TrimSpace(c.PostForm("comment"))
	if err := validateInternalDecision(decision, comment); err != nil {
		c.String(http.StatusUnprocessableEntity, err.Error())
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать сохранение визы")
		return
	}
	defer tx.Rollback(c.Request.Context())
	var questionTitle, questionStatus string
	err = tx.QueryRow(c.Request.Context(), `
		SELECT title, status FROM questions WHERE id = $1 FOR UPDATE
	`, questionID).Scan(&questionTitle, &questionStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Вопрос не найден")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить вопрос")
		return
	}
	if questionStatus != "internal_review" {
		c.String(http.StatusConflict, "Внутреннее согласование вопроса не активно")
		return
	}
	var roundID, fileID int64
	var versionNo int
	var service, requirementStatus, roundStatus, fileTitle string
	var filePaused bool
	err = tx.QueryRow(c.Request.Context(), `
		SELECT r.round_id, r.question_file_id, r.version_no, r.internal_service, r.status,
		       rr.status, f.title,
		       EXISTS (SELECT 1 FROM question_file_versions pending
		               WHERE pending.question_file_id = r.question_file_id AND pending.approval_status = 'pending')
		FROM internal_review_requirements r
		JOIN internal_review_rounds rr ON rr.id = r.round_id
		JOIN question_files f ON f.id = r.question_file_id
		WHERE r.id = $1 AND rr.question_id = $2
		FOR UPDATE OF r, rr
	`, requirementID, questionID).Scan(&roundID, &fileID, &versionNo, &service, &requirementStatus, &roundStatus, &fileTitle, &filePaused)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Задание на согласование не найдено")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить задание")
		return
	}
	if roundStatus != "active" || requirementStatus != "pending" {
		c.String(http.StatusConflict, "По этому заданию уже нельзя отправить визу")
		return
	}
	if filePaused {
		c.String(http.StatusConflict, "Согласование этого файла приостановлено до решения секретаря по новой версии")
		return
	}
	if usr.InternalService != service {
		c.String(http.StatusForbidden, "Это задание относится к другой дирекции")
		return
	}
	var visaID int64
	err = tx.QueryRow(c.Request.Context(), `
		INSERT INTO internal_review_visas (requirement_id, decision, comment, decided_by)
		VALUES ($1, $2, $3, $4) RETURNING id
	`, requirementID, decision, comment, usr.ID).Scan(&visaID)
	if err == nil {
		_, err = tx.Exec(c.Request.Context(), `UPDATE internal_review_requirements SET status = 'responded' WHERE id = $1`, requirementID)
	}
	if err == nil {
		err = app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
			EventType: "internal_review.visa_submitted", TargetType: "internal_visa", TargetID: &visaID,
			TargetLabel: fileTitle, QuestionID: &questionID, VersionNo: &versionNo,
			Details: internalServiceLabel(service) + ": " + internalDecisionLabel(decision),
		})
	}
	if err == nil {
		err = app.finishInternalReviewIfComplete(c.Request.Context(), tx, usr, questionID, roundID, questionTitle)
	}
	if err == nil {
		err = tx.Commit(c.Request.Context())
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось сохранить визу")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", questionID))
}

func (app *application) finishInternalReviewIfComplete(ctx context.Context, tx pgx.Tx, actor user, questionID, roundID int64, questionTitle string) error {
	var pendingUploads int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM question_file_versions v
		JOIN question_files f ON f.id = v.question_file_id
		WHERE f.question_id = $1 AND f.status = 'active' AND v.approval_status = 'pending'
	`, questionID).Scan(&pendingUploads); err != nil || pendingUploads > 0 {
		return err
	}
	var pending, rejected int
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE r.status = 'pending'),
		       COUNT(*) FILTER (WHERE r.status = 'responded' AND v.decision = 'rejected')
		FROM internal_review_requirements r
		LEFT JOIN internal_review_visas v ON v.requirement_id = r.id AND v.withdrawn_at IS NULL
		WHERE r.round_id = $1 AND r.status <> 'superseded'
	`, roundID).Scan(&pending, &rejected)
	if err != nil || pending > 0 {
		return err
	}
	outcome := "ready_for_committee"
	if rejected > 0 {
		outcome = "revision_required"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE internal_review_rounds SET status = 'completed', outcome = $2, completed_at = NOW()
		WHERE id = $1 AND status = 'active'
	`, roundID, outcome); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE questions SET status = $2, updated_at = NOW() WHERE id = $1`, questionID, outcome); err != nil {
		return err
	}
	return app.writeAudit(ctx, tx, actor, auditRecord{
		EventType: "internal_review.completed", TargetType: "question", TargetID: &questionID,
		TargetLabel: questionTitle, QuestionID: &questionID,
		Details: fmt.Sprintf("Раунд №%d завершён: %s", roundID, questionStatusLabel(outcome)),
	})
}

func (app *application) finishActiveInternalReviewIfComplete(ctx context.Context, tx pgx.Tx, actor user, questionID int64) error {
	var roundID int64
	var questionTitle string
	err := tx.QueryRow(ctx, `
		SELECT rr.id, q.title FROM internal_review_rounds rr
		JOIN questions q ON q.id = rr.question_id
		WHERE rr.question_id = $1 AND rr.status = 'active' FOR UPDATE OF rr, q
	`, questionID).Scan(&roundID, &questionTitle)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return app.finishInternalReviewIfComplete(ctx, tx, actor, questionID, roundID, questionTitle)
}

func replaceActiveInternalReviewFileVersion(ctx context.Context, tx pgx.Tx, questionID, fileID int64, versionNo int) error {
	var roundID int64
	err := tx.QueryRow(ctx, `
		SELECT id FROM internal_review_rounds
		WHERE question_id = $1 AND status = 'active'
		FOR UPDATE
	`, questionID).Scan(&roundID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE internal_review_requirements SET status = 'superseded'
		WHERE round_id = $1 AND question_file_id = $2 AND version_no <> $3
	`, roundID, fileID, versionNo); err != nil {
		return err
	}
	for _, service := range requiredInternalServices {
		if _, err := tx.Exec(ctx, `
			INSERT INTO internal_review_requirements (
				round_id, question_file_id, version_no, internal_service
			) VALUES ($1, $2, $3, $4)
			ON CONFLICT (round_id, question_file_id, version_no, internal_service) DO NOTHING
		`, roundID, fileID, versionNo, service); err != nil {
			return err
		}
	}
	return nil
}

func (app *application) withdrawInternalVisa(c *gin.Context) {
	usr := c.MustGet("user").(user)
	questionID, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	visaID, ok := parsePositiveID(c, "visaID", "Некорректный идентификатор визы")
	if !ok {
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать отзыв визы")
		return
	}
	defer tx.Rollback(c.Request.Context())
	var requirementID int64
	var versionNo int
	var decidedBy int64
	var decidedAt time.Time
	var service, roundStatus, fileTitle string
	err = tx.QueryRow(c.Request.Context(), `
		SELECT v.requirement_id, v.decided_by, v.decided_at, r.version_no,
		       r.internal_service, rr.status, f.title
		FROM internal_review_visas v
		JOIN internal_review_requirements r ON r.id = v.requirement_id
		JOIN internal_review_rounds rr ON rr.id = r.round_id
		JOIN question_files f ON f.id = r.question_file_id
		WHERE v.id = $1 AND rr.question_id = $2 AND v.withdrawn_at IS NULL
		FOR UPDATE OF v, r, rr
	`, visaID, questionID).Scan(&requirementID, &decidedBy, &decidedAt, &versionNo, &service, &roundStatus, &fileTitle)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Действующая виза не найдена")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить визу")
		return
	}
	if decidedBy != usr.ID || service != usr.InternalService {
		c.String(http.StatusForbidden, "Отозвать визу может только отправивший её пользователь")
		return
	}
	if roundStatus != "active" {
		c.String(http.StatusConflict, "Завершённый раунд изменить нельзя")
		return
	}
	if time.Now().After(decidedAt.Add(24 * time.Hour)) {
		c.String(http.StatusConflict, "С момента отправки визы прошло более 24 часов")
		return
	}
	_, err = tx.Exec(c.Request.Context(), `UPDATE internal_review_visas SET withdrawn_by = $2, withdrawn_at = NOW() WHERE id = $1`, visaID, usr.ID)
	if err == nil {
		_, err = tx.Exec(c.Request.Context(), `UPDATE internal_review_requirements SET status = 'pending' WHERE id = $1`, requirementID)
	}
	if err == nil {
		err = app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
			EventType: "internal_review.visa_withdrawn", TargetType: "internal_visa", TargetID: &visaID,
			TargetLabel: fileTitle, QuestionID: &questionID, VersionNo: &versionNo,
			Details: "Отозвана виза: " + internalServiceLabel(service),
		})
	}
	if err == nil {
		err = tx.Commit(c.Request.Context())
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось отозвать визу")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", questionID))
}

func (app *application) extendInternalReview(c *gin.Context) {
	usr := c.MustGet("user").(user)
	questionID, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	newDeadline, err := time.Parse("2006-01-02", strings.TrimSpace(c.PostForm("deadline")))
	reason := strings.TrimSpace(c.PostForm("reason"))
	if err != nil || reason == "" || len([]rune(reason)) > 2000 {
		c.String(http.StatusUnprocessableEntity, "Укажите новый срок и причину длиной до 2 000 знаков")
		return
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if newDeadline.Before(today) {
		c.String(http.StatusUnprocessableEntity, "Новый срок не может быть в прошлом")
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать продление срока")
		return
	}
	defer tx.Rollback(c.Request.Context())
	var roundID int64
	var oldDeadline time.Time
	var title string
	err = tx.QueryRow(c.Request.Context(), `
		SELECT rr.id, rr.deadline, q.title
		FROM internal_review_rounds rr JOIN questions q ON q.id = rr.question_id
		WHERE rr.question_id = $1 AND rr.status = 'active' FOR UPDATE OF rr, q
	`, questionID).Scan(&roundID, &oldDeadline, &title)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusConflict, "Активное внутреннее согласование не найдено")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить срок согласования")
		return
	}
	if !newDeadline.After(oldDeadline) {
		c.String(http.StatusUnprocessableEntity, "Новый срок должен быть позже текущего")
		return
	}
	_, err = tx.Exec(c.Request.Context(), `UPDATE internal_review_rounds SET deadline = $2 WHERE id = $1`, roundID, newDeadline)
	if err == nil {
		_, err = tx.Exec(c.Request.Context(), `UPDATE questions SET internal_deadline = $2, updated_at = NOW() WHERE id = $1`, questionID, newDeadline)
	}
	if err == nil {
		err = app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
			EventType: "internal_review.extended", TargetType: "question", TargetID: &questionID,
			TargetLabel: title, QuestionID: &questionID,
			Details: fmt.Sprintf("Срок изменён с %s на %s. Причина: %s", oldDeadline.Format("02.01.2006"), newDeadline.Format("02.01.2006"), reason),
		})
	}
	if err == nil {
		err = tx.Commit(c.Request.Context())
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось продлить срок")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", questionID))
}

func (app *application) cancelInternalReview(c *gin.Context) {
	usr := c.MustGet("user").(user)
	questionID, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать отмену согласования")
		return
	}
	defer tx.Rollback(c.Request.Context())
	var roundID int64
	var title string
	err = tx.QueryRow(c.Request.Context(), `
		SELECT rr.id, q.title FROM internal_review_rounds rr
		JOIN questions q ON q.id = rr.question_id
		WHERE rr.question_id = $1 AND rr.status = 'active' FOR UPDATE OF rr, q
	`, questionID).Scan(&roundID, &title)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusConflict, "Активное внутреннее согласование не найдено")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить согласование")
		return
	}
	_, err = tx.Exec(c.Request.Context(), `UPDATE internal_review_rounds SET status = 'cancelled', cancelled_at = NOW() WHERE id = $1`, roundID)
	if err == nil {
		_, err = tx.Exec(c.Request.Context(), `UPDATE questions SET status = 'draft', updated_at = NOW() WHERE id = $1`, questionID)
	}
	if err == nil {
		err = app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
			EventType: "internal_review.cancelled", TargetType: "question", TargetID: &questionID,
			TargetLabel: title, QuestionID: &questionID, Details: fmt.Sprintf("Отменён раунд №%d", roundID),
		})
	}
	if err == nil {
		err = tx.Commit(c.Request.Context())
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось отменить согласование")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", questionID))
}

func parsePositiveID(c *gin.Context, parameter, message string) (int64, bool) {
	value, err := strconv.ParseInt(c.Param(parameter), 10, 64)
	if err != nil || value < 1 {
		c.String(http.StatusBadRequest, message)
		return 0, false
	}
	return value, true
}

func (app *application) loadInternalReview(ctx context.Context, questionID int64, questionStatus string, usr user) (internalReviewView, error) {
	view := internalReviewView{CanStart: usr.Role == "secretary" && questionStatus == "draft"}
	var roundID int64
	var status, outcome string
	var deadline time.Time
	err := app.db.QueryRow(ctx, `
		SELECT id, status, COALESCE(outcome, ''), deadline
		FROM internal_review_rounds WHERE question_id = $1
		ORDER BY started_at DESC, id DESC LIMIT 1
	`, questionID).Scan(&roundID, &status, &outcome, &deadline)
	if errors.Is(err, pgx.ErrNoRows) {
		return view, nil
	}
	if err != nil {
		return internalReviewView{}, err
	}
	view.HasRound = true
	view.Active = status == "active"
	view.CanManage = view.Active && usr.Role == "secretary"
	view.DeadlineLabel = deadline.Format("02.01.2006")
	today := time.Now()
	view.PastDue = view.Active && deadline.Before(time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location()))
	switch status {
	case "active":
		view.StatusLabel = "Идёт согласование"
	case "cancelled":
		view.StatusLabel = "Раунд отменён"
	default:
		view.StatusLabel = questionStatusLabel(outcome)
	}
	rows, err := app.db.Query(ctx, `
		SELECT r.id, f.title, r.version_no, r.internal_service, r.status,
		       EXISTS (SELECT 1 FROM question_file_versions pending
		               WHERE pending.question_file_id = r.question_file_id AND pending.approval_status = 'pending')
		FROM internal_review_requirements r
		JOIN question_files f ON f.id = r.question_file_id
		WHERE r.round_id = $1 AND r.status <> 'superseded'
		ORDER BY f.created_at, f.id,
			CASE r.internal_service WHEN 'legal' THEN 1 WHEN 'finance' THEN 2 WHEN 'construction' THEN 3 ELSE 4 END
	`, roundID)
	if err != nil {
		return internalReviewView{}, err
	}
	indexes := make(map[int64]int)
	responded := 0
	for rows.Next() {
		var item internalReviewItem
		var requirementStatus string
		if err := rows.Scan(&item.RequirementID, &item.FileTitle, &item.VersionNo, &item.Service, &requirementStatus, &item.FilePaused); err != nil {
			rows.Close()
			return internalReviewView{}, err
		}
		item.ServiceLabel = internalServiceLabel(item.Service)
		item.CanRespond = view.Active && !item.FilePaused && requirementStatus == "pending" && usr.Role == "approver" && usr.InternalService == item.Service
		if requirementStatus == "responded" {
			responded++
		}
		indexes[item.RequirementID] = len(view.Items)
		view.Items = append(view.Items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return internalReviewView{}, err
	}
	rows.Close()
	view.ProgressLabel = fmt.Sprintf("%d из %d", responded, len(view.Items))
	visaRows, err := app.db.Query(ctx, `
		SELECT v.id, v.requirement_id, v.decision, v.comment, v.decided_by, u.full_name, v.decided_at,
		       v.withdrawn_at IS NOT NULL, COALESCE(v.withdrawn_at, 'epoch'::timestamptz)
		FROM internal_review_visas v JOIN users u ON u.id = v.decided_by
		JOIN internal_review_requirements r ON r.id = v.requirement_id
		WHERE r.round_id = $1 ORDER BY v.decided_at DESC, v.id DESC
	`, roundID)
	if err != nil {
		return internalReviewView{}, err
	}
	defer visaRows.Close()
	for visaRows.Next() {
		var requirementID int64
		var decidedByID int64
		var decision string
		var decidedAt, withdrawnAt time.Time
		var visa internalVisaItem
		if err := visaRows.Scan(&visa.ID, &requirementID, &decision, &visa.Comment, &decidedByID, &visa.DecidedBy,
			&decidedAt, &visa.Withdrawn, &withdrawnAt); err != nil {
			return internalReviewView{}, err
		}
		index, ok := indexes[requirementID]
		if !ok {
			continue
		}
		visa.DecisionLabel = internalDecisionLabel(decision)
		visa.DecidedLabel = decidedAt.Format("02.01.2006 15:04")
		if visa.Withdrawn {
			visa.WithdrawnLabel = withdrawnAt.Format("02.01.2006 15:04")
			view.Items[index].History = append(view.Items[index].History, visa)
			continue
		}
		visa.CanWithdraw = view.Active && decidedByID == usr.ID && usr.Role == "approver" && usr.InternalService == view.Items[index].Service && !time.Now().After(decidedAt.Add(24*time.Hour))
		view.Items[index].HasVisa = true
		view.Items[index].Visa = visa
	}
	return view, visaRows.Err()
}
