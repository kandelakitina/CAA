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

var errFileExclusionConflict = errors.New("Исключение доступно только между раундами, до положительного решения и отмены вопроса")

func canExcludeQuestionFile(status string) bool {
	switch status {
	case "draft", "revision_required", "ready_for_committee", "rejected", "no_quorum":
		return true
	default:
		return false
	}
}

func validateFileExclusionReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len([]rune(reason)) > 2000 {
		return "", errors.New("Укажите причину исключения длиной от 1 до 2000 знаков")
	}
	return reason, nil
}

func (app *application) excludeQuestionFile(c *gin.Context) {
	questionID, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	fileID, ok := parsePositiveID(c, "fileID", "Некорректный идентификатор файла")
	if !ok {
		return
	}
	reason, err := validateFileExclusionReason(c.PostForm("reason"))
	if err != nil {
		c.String(http.StatusUnprocessableEntity, err.Error())
		return
	}
	expected, err := time.Parse(time.RFC3339Nano, c.PostForm("question_context"))
	if err != nil {
		c.String(http.StatusConflict, errQuestionEditConflict.Error())
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать исключение файла")
		return
	}
	err = app.excludeQuestionFileTransaction(c.Request.Context(), tx, c.MustGet("user").(user), questionID, fileID, reason, expected)
	var rule *fileExclusionRuleError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		c.String(http.StatusNotFound, "Вопрос или файл не найден")
	case errors.Is(err, errFileExclusionConflict), errors.Is(err, errQuestionEditConflict):
		c.String(http.StatusConflict, err.Error())
	case errors.As(err, &rule):
		c.String(rule.Status, rule.Message)
	case err != nil:
		c.String(http.StatusInternalServerError, "Не удалось исключить файл")
	default:
		c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", questionID))
	}
}

type fileExclusionRuleError struct {
	Status  int
	Message string
}

func (e *fileExclusionRuleError) Error() string { return e.Message }

func (app *application) excludeQuestionFileTransaction(ctx context.Context, tx pgx.Tx, actor user, questionID, fileID int64, reason string, expected time.Time) error {
	defer tx.Rollback(ctx)
	var status, questionType string
	var updatedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT status, question_type, updated_at FROM questions WHERE id = $1 FOR UPDATE`, questionID).Scan(&status, &questionType, &updatedAt); err != nil {
		return err
	}
	if !canExcludeQuestionFile(status) {
		return errFileExclusionConflict
	}
	if !updatedAt.Equal(expected) {
		return errQuestionEditConflict
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM internal_review_rounds WHERE question_id = $1 AND status = 'active')
		OR EXISTS (SELECT 1 FROM committee_vote_rounds WHERE question_id = $1 AND status = 'active')`, questionID).Scan(&active); err != nil {
		return err
	}
	if active {
		return errFileExclusionConflict
	}
	var title, fileStatus string
	var version int
	if err := tx.QueryRow(ctx, `SELECT title, status, COALESCE(current_version_no, 0) FROM question_files
		WHERE id = $1 AND question_id = $2 FOR UPDATE`, fileID, questionID).Scan(&title, &fileStatus, &version); err != nil {
		return err
	}
	if fileStatus != "active" {
		return &fileExclusionRuleError{409, "Файл уже исключён из комплекта"}
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM question_file_versions WHERE question_file_id = $1 AND approval_status = 'pending')`, fileID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return &fileExclusionRuleError{409, "Сначала подтвердите или отклоните ожидающую версию этого файла"}
	}
	// Never-official rejected uploads can be excluded even from an incomplete draft.
	// Removing an official file must leave a complete official bundle.
	if version > 0 {
		rows, err := tx.Query(ctx, `SELECT id, current_version_no, category FROM question_files
			WHERE question_id = $1 AND id <> $2 AND status = 'active' AND current_version_no IS NOT NULL ORDER BY id`, questionID, fileID)
		if err != nil {
			return err
		}
		var remaining []reviewStartFile
		for rows.Next() {
			var file reviewStartFile
			if err := rows.Scan(&file.ID, &file.VersionNo, &file.Category); err != nil {
				rows.Close()
				return err
			}
			remaining = append(remaining, file)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if err := validateQuestionBundle(questionType, remaining); err != nil {
			return &fileExclusionRuleError{422, "После исключения комплект будет неполным. " + err.Error()}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE question_files SET status = 'excluded', exclusion_reason = $2,
		excluded_at = clock_timestamp(), excluded_by_name = $3, updated_at = clock_timestamp() WHERE id = $1`, fileID, reason, actor.FullName); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE questions SET status = 'draft', updated_at = clock_timestamp() WHERE id = $1`, questionID); err != nil {
		return err
	}
	var versionNo *int
	if version > 0 {
		versionNo = &version
	}
	if err := app.writeAudit(ctx, tx, actor, auditRecord{
		EventType: "question.file_excluded", TargetType: "question_file", TargetID: &fileID,
		TargetLabel: title, QuestionID: &questionID, VersionNo: versionNo,
		Details: fmt.Sprintf("Причина: %s. Предыдущий статус вопроса: %s. Вопрос возвращён в черновик для полного внутреннего согласования", reason, questionStatusLabel(status)),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
