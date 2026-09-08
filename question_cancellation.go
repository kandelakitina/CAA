package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

var errQuestionCannotCancel = errors.New("Отменить можно только вопрос без положительного решения, который ещё не отменён")

func canCancelQuestion(status string) bool {
	switch status {
	case "draft", "internal_review", "revision_required", "ready_for_committee", "committee_voting", "rejected", "no_quorum":
		return true
	default:
		return false
	}
}

func validateQuestionCancellationReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len([]rune(reason)) > 2000 {
		return "", errors.New("Укажите причину отмены длиной от 1 до 2000 знаков")
	}
	return reason, nil
}

func (app *application) cancelQuestion(c *gin.Context) {
	questionID, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	reason, err := validateQuestionCancellationReason(c.PostForm("reason"))
	if err != nil {
		respondMessage(c, http.StatusUnprocessableEntity, err.Error())
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		respondMessage(c, http.StatusInternalServerError, "Не удалось начать отмену вопроса")
		return
	}
	err = app.cancelQuestionTransaction(c.Request.Context(), tx, c.MustGet("user").(user), questionID, reason)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		respondMessage(c, http.StatusNotFound, "Вопрос не найден")
	case errors.Is(err, errQuestionCannotCancel):
		respondMessage(c, http.StatusConflict, err.Error())
	case err != nil:
		respondMessage(c, http.StatusInternalServerError, "Не удалось отменить вопрос")
	default:
		c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", questionID))
	}
}

// Lock the question before changing its rounds, as upload and vote handlers do.
// Cancellation and audit must either be committed together or rolled back.
func (app *application) cancelQuestionTransaction(ctx context.Context, tx pgx.Tx, actor user, questionID int64, reason string) error {
	defer tx.Rollback(ctx)
	var title, status string
	if err := tx.QueryRow(ctx, `SELECT title, status FROM questions WHERE id = $1 FOR UPDATE`, questionID).Scan(&title, &status); err != nil {
		return err
	}
	if !canCancelQuestion(status) {
		return errQuestionCannotCancel
	}
	for _, round := range []struct{ table, event string }{
		{"internal_review_rounds", "internal_review.cancelled"},
		{"committee_vote_rounds", "committee_vote.cancelled"},
	} {
		// Table names are constants, never request data.
		var roundID int64
		err := tx.QueryRow(ctx, `UPDATE `+round.table+` SET status = 'cancelled', cancelled_at = NOW()
			WHERE question_id = $1 AND status = 'active' RETURNING id`, questionID).Scan(&roundID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if err := app.writeAudit(ctx, tx, actor, auditRecord{
			EventType: round.event, TargetType: "question", TargetID: &questionID,
			TargetLabel: title, QuestionID: &questionID,
			Details: fmt.Sprintf("Раунд №%d отменён вместе с вопросом. Причина: %s", roundID, reason),
		}); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE questions SET status = 'cancelled', cancellation_reason = $2,
		cancelled_at = NOW(), cancelled_by_name = $3, updated_at = NOW() WHERE id = $1`, questionID, reason, actor.FullName); err != nil {
		return err
	}
	if err := app.writeAudit(ctx, tx, actor, auditRecord{
		EventType: "question.cancelled", TargetType: "question", TargetID: &questionID,
		TargetLabel: title, QuestionID: &questionID,
		Details: fmt.Sprintf("Предыдущий статус: %s. Причина: %s", questionStatusLabel(status), reason),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
