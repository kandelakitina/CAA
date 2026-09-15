package main

import (
	"context"
	"fmt"
	"strings"
)

func (app *application) invalidateEmailTokens(ctx context.Context, tx auditExecutor, r auditRecord) error {
	switch r.EventType {
	case "user.password_changed", "user.password_reset", "user.deactivated", "user.restored", "user.sessions_revoked":
		if r.TargetID != nil {
			_, err := tx.Exec(ctx, `UPDATE email_password_tokens SET used_at=NOW() WHERE user_id=$1 AND used_at IS NULL`, *r.TargetID)
			return err
		}
	}
	return nil
}

func workflowMailEvent(event string) bool {
	return strings.HasPrefix(event, "internal_review.") || strings.HasPrefix(event, "committee_vote.") || event == "question.cancelled" || event == "question.file_version_uploaded" || event == "question.file_version_confirmed" || event == "question.file_version_rejected" || event == "question.decision_revised"
}
func (app *application) queueWorkflowMail(ctx context.Context, tx auditExecutor, actor user, r auditRecord) error {
	if app.mail == nil || !workflowMailEvent(r.EventType) {
		return nil
	}
	path := ""
	if r.QuestionID != nil {
		path = fmt.Sprintf("/questions/%d", *r.QuestionID)
	} else {
		return nil
	}
	// Do not include comments, attachments or document contents in mail.
	subject := auditEventLabel(r.EventType) + " — Neva Approvals"
	body := subject + "\n\nОткройте карточку для просмотра события и актуального состояния:\n" + app.mail.BaseURL + path
	encrypted, err := app.encryptMail(body)
	if err != nil {
		return err
	}
	// Managers receive all supported changes. Internal approvers receive internal
	// review events. Committee recipients are taken from actual participation,
	// and only when the question is visible to their role.
	_, err = tx.Exec(ctx, `INSERT INTO email_outbox(user_id,subject,encrypted_body)
 SELECT u.id,$1,$2 FROM users u WHERE u.active=TRUE AND u.id<>$3 AND (
 u.role IN ('admin','secretary')
 OR ($4::BIGINT IS NOT NULL AND u.role='approver' AND $5 AND EXISTS (
 SELECT 1 FROM internal_review_requirements req JOIN internal_review_rounds round ON round.id=req.round_id
 WHERE round.question_id=$4 AND req.internal_service=u.internal_service
 AND round.id=(SELECT MAX(id) FROM internal_review_rounds WHERE question_id=$4)))
 OR ($4::BIGINT IS NOT NULL AND u.role='committee' AND $6 AND EXISTS (
 SELECT 1 FROM committee_vote_participants p JOIN committee_vote_rounds round ON round.id=p.round_id
 JOIN questions q ON q.id=round.question_id WHERE q.id=$4 AND p.user_id=u.id
 AND round.id=(SELECT MAX(id) FROM committee_vote_rounds WHERE question_id=$4)
 AND (q.status IN ('committee_voting','approved','rejected','no_quorum','cancelled'))))
 )`, subject, encrypted, actor.ID, r.QuestionID, !strings.HasPrefix(r.EventType, "committee_vote."), !strings.HasPrefix(r.EventType, "internal_review."))
	return err
}
