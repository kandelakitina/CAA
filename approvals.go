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

type approvalCandidate struct {
	ID        int64
	FullName  string
	Email     string
	RoleLabel string
}

type approvalParticipantView struct {
	UserID        int64
	FullName      string
	Role          string
	RoleLabel     string
	Decision      string
	DecisionLabel string
	Comment       string
	RespondedAt   string
	HasResponded  bool
}

type approvalRoundView struct {
	ID           int64
	VersionNo    int
	Status       string
	StatusLabel  string
	StartedBy    string
	StartedAt    string
	Deadline     string
	FinalOutcome string
	OutcomeLabel string
	FinalComment string
	IsActive     bool
	AllResponded bool
	PendingCount int
	Participants []approvalParticipantView
}

type approvalView struct {
	Candidates         []approvalCandidate
	Round              *approvalRoundView
	CurrentParticipant *approvalParticipantView
	CanManage          bool
	CanStart           bool
}

func (app *application) loadApprovalView(ctx context.Context, documentID int64, currentVersion int, documentStatus string, usr user) (approvalView, error) {
	view := approvalView{CanManage: usr.Role == "admin" || usr.Role == "secretary"}
	rows, err := app.db.Query(ctx, `
		SELECT id, full_name, email, role
		FROM users
		WHERE active = TRUE AND role IN ('committee', 'approver')
		ORDER BY role, full_name, id
	`)
	if err != nil {
		return view, err
	}
	for rows.Next() {
		var candidate approvalCandidate
		var role string
		if err := rows.Scan(&candidate.ID, &candidate.FullName, &candidate.Email, &role); err != nil {
			rows.Close()
			return view, err
		}
		candidate.RoleLabel = roleLabel(role)
		view.Candidates = append(view.Candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return view, err
	}
	rows.Close()

	var round approvalRoundView
	var startedAt time.Time
	err = app.db.QueryRow(ctx, `
		SELECT r.id, r.version_no, r.status, u.full_name, r.started_at,
		       COALESCE(TO_CHAR(r.deadline, 'DD.MM.YYYY'), ''),
		       COALESCE(r.final_outcome, ''), r.final_comment
		FROM approval_rounds r
		JOIN users u ON u.id = r.started_by
		WHERE r.document_id = $1
		ORDER BY r.started_at DESC, r.id DESC
		LIMIT 1
	`, documentID).Scan(
		&round.ID, &round.VersionNo, &round.Status, &round.StartedBy, &startedAt,
		&round.Deadline, &round.FinalOutcome, &round.FinalComment,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		view.CanStart = view.CanManage && documentStatus == "draft" && len(view.Candidates) > 0
		return view, nil
	}
	if err != nil {
		return view, err
	}
	round.StartedAt = startedAt.Format("02.01.2006 15:04")
	round.IsActive = round.Status == "active"
	round.StatusLabel = approvalRoundStatusLabel(round.Status)
	round.OutcomeLabel = approvalOutcomeLabel(round.FinalOutcome)

	participantRows, err := app.db.Query(ctx, `
		SELECT p.user_id, u.full_name, p.role_snapshot,
		       COALESCE(p.decision, ''), p.comment,
		       COALESCE(TO_CHAR(p.responded_at, 'DD.MM.YYYY HH24:MI'), '')
		FROM approval_participants p
		JOIN users u ON u.id = p.user_id
		WHERE p.round_id = $1
		ORDER BY p.role_snapshot, u.full_name, p.id
	`, round.ID)
	if err != nil {
		return view, err
	}
	defer participantRows.Close()
	for participantRows.Next() {
		var item approvalParticipantView
		if err := participantRows.Scan(
			&item.UserID, &item.FullName, &item.Role, &item.Decision,
			&item.Comment, &item.RespondedAt,
		); err != nil {
			return view, err
		}
		item.RoleLabel = roleLabel(item.Role)
		item.HasResponded = item.Decision != ""
		item.DecisionLabel = approvalDecisionLabel(item.Role, item.Decision)
		if !item.HasResponded {
			round.PendingCount++
		}
		round.Participants = append(round.Participants, item)
		if item.UserID == usr.ID {
			copy := item
			view.CurrentParticipant = &copy
		}
	}
	if err := participantRows.Err(); err != nil {
		return view, err
	}
	round.AllResponded = len(round.Participants) > 0 && round.PendingCount == 0
	view.Round = &round
	view.CanStart = view.CanManage && !round.IsActive && (round.VersionNo != currentVersion || round.Status == "cancelled") && documentStatus == "draft" && len(view.Candidates) > 0
	return view, nil
}

func (app *application) startApproval(c *gin.Context) {
	documentID, ok := approvalDocumentID(c)
	if !ok {
		return
	}
	usr := c.MustGet("user").(user)
	participantIDs, err := parseParticipantIDs(c.PostFormArray("participant_ids"))
	if err != nil || len(participantIDs) == 0 {
		app.renderDocument(c, http.StatusUnprocessableEntity, documentID, "Выберите хотя бы одного члена комитета или согласующего")
		return
	}

	var deadline any
	if value := strings.TrimSpace(c.PostForm("deadline")); value != "" {
		parsed, err := time.Parse("2006-01-02", value)
		if err != nil {
			app.renderDocument(c, http.StatusUnprocessableEntity, documentID, "Укажите корректный срок согласования")
			return
		}
		deadline = parsed
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	tx, err := app.db.Begin(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать согласование")
		return
	}
	defer tx.Rollback(ctx)

	var versionNo int
	var documentStatus string
	var documentTitle string
	err = tx.QueryRow(ctx, `
		SELECT current_version, status, title FROM documents WHERE id = $1 FOR UPDATE
	`, documentID).Scan(&versionNo, &documentStatus, &documentTitle)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Документ не найден")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось прочитать документ")
		return
	}
	if documentStatus != "draft" {
		app.renderDocument(c, http.StatusConflict, documentID, "Запустить согласование можно только для версии в статусе «Черновик»")
		return
	}
	var activeCount int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM approval_rounds WHERE document_id = $1 AND status = 'active'`, documentID).Scan(&activeCount); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось проверить согласование")
		return
	}
	if activeCount > 0 {
		app.renderDocument(c, http.StatusConflict, documentID, "По документу уже идёт согласование")
		return
	}

	userRows, err := tx.Query(ctx, `
		SELECT id, role FROM users
		WHERE id = ANY($1) AND active = TRUE AND role IN ('committee', 'approver')
	`, participantIDs)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось проверить участников")
		return
	}
	roles := make(map[int64]string)
	for userRows.Next() {
		var id int64
		var role string
		if err := userRows.Scan(&id, &role); err != nil {
			userRows.Close()
			c.String(http.StatusInternalServerError, "Не удалось прочитать участников")
			return
		}
		roles[id] = role
	}
	if err := userRows.Err(); err != nil {
		userRows.Close()
		c.String(http.StatusInternalServerError, "Не удалось проверить список участников")
		return
	}
	userRows.Close()
	if len(roles) != len(participantIDs) {
		app.renderDocument(c, http.StatusUnprocessableEntity, documentID, "Один из выбранных пользователей недоступен или имеет неподходящую роль")
		return
	}

	var roundID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO approval_rounds (document_id, version_no, started_by, deadline)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, documentID, versionNo, usr.ID, deadline).Scan(&roundID)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось создать раунд согласования")
		return
	}
	for _, id := range participantIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO approval_participants (round_id, user_id, role_snapshot)
			VALUES ($1, $2, $3)
		`, roundID, id, roles[id]); err != nil {
			c.String(http.StatusInternalServerError, "Не удалось назначить участников")
			return
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE documents SET status = 'in_review', updated_at = NOW() WHERE id = $1`, documentID); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось изменить статус документа")
		return
	}
	if err := app.writeAudit(ctx, tx, usr, auditRecord{
		EventType: "approval.started", TargetType: "approval_round", TargetID: &roundID,
		TargetLabel: documentTitle, DocumentID: &documentID, VersionNo: &versionNo,
		Details: fmt.Sprintf("Назначено участников: %d", len(participantIDs)),
	}); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось записать событие аудита")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось запустить согласование")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/documents/%d", documentID))
}

func (app *application) respondToApproval(c *gin.Context) {
	documentID, ok := approvalDocumentID(c)
	if !ok {
		return
	}
	usr := c.MustGet("user").(user)
	decision := c.PostForm("decision")
	comment := strings.TrimSpace(c.PostForm("comment"))

	var role string
	err := app.db.QueryRow(c.Request.Context(), `
		SELECT p.role_snapshot
		FROM approval_participants p
		JOIN approval_rounds r ON r.id = p.round_id
		WHERE r.document_id = $1 AND r.status = 'active' AND p.user_id = $2 AND p.decision IS NULL
	`, documentID, usr.ID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusConflict, "Для вас нет ожидающего ответа по этому документу")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось проверить назначение")
		return
	}
	if !validApprovalDecision(role, decision) {
		app.renderDocument(c, http.StatusUnprocessableEntity, documentID, "Выберите допустимое решение")
		return
	}
	if decision != "approve" && comment == "" {
		app.renderDocument(c, http.StatusUnprocessableEntity, documentID, "Для этого решения укажите комментарий")
		return
	}
	if len([]rune(comment)) > 5000 {
		app.renderDocument(c, http.StatusUnprocessableEntity, documentID, "Комментарий не должен превышать 5000 знаков")
		return
	}

	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать сохранение решения")
		return
	}
	defer tx.Rollback(c.Request.Context())

	command, err := tx.Exec(c.Request.Context(), `
		UPDATE approval_participants p
		SET decision = $3, comment = $4, responded_at = NOW()
		FROM approval_rounds r
		WHERE p.round_id = r.id AND r.document_id = $1 AND r.status = 'active'
		  AND p.user_id = $2 AND p.decision IS NULL
	`, documentID, usr.ID, decision, comment)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось сохранить решение")
		return
	}
	if command.RowsAffected() != 1 {
		c.String(http.StatusConflict, "Решение уже было сохранено или согласование закрыто")
		return
	}
	var roundID int64
	var versionNo int
	var documentTitle string
	if err := tx.QueryRow(c.Request.Context(), `
		SELECT r.id, r.version_no, d.title
		FROM approval_rounds r
		JOIN documents d ON d.id = r.document_id
		WHERE r.document_id = $1 AND r.status = 'active'
	`, documentID).Scan(&roundID, &versionNo, &documentTitle); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось определить согласование")
		return
	}
	if err := app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
		EventType: "approval.response_submitted", TargetType: "approval_round", TargetID: &roundID,
		TargetLabel: documentTitle, DocumentID: &documentID, VersionNo: &versionNo,
		Details: approvalDecisionLabel(role, decision),
	}); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось записать событие аудита")
		return
	}
	if err := tx.Commit(c.Request.Context()); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось сохранить решение")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/documents/%d", documentID))
}

func (app *application) completeApproval(c *gin.Context) {
	documentID, ok := approvalDocumentID(c)
	if !ok {
		return
	}
	outcome := c.PostForm("outcome")
	comment := strings.TrimSpace(c.PostForm("final_comment"))
	if outcome != "approved" && outcome != "rejected" {
		app.renderDocument(c, http.StatusUnprocessableEntity, documentID, "Выберите итог согласования")
		return
	}
	if len([]rune(comment)) > 5000 {
		app.renderDocument(c, http.StatusUnprocessableEntity, documentID, "Итоговый комментарий не должен превышать 5000 знаков")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	tx, err := app.db.Begin(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось завершить согласование")
		return
	}
	defer tx.Rollback(ctx)
	var roundID int64
	var versionNo int
	var documentTitle string
	err = tx.QueryRow(ctx, `
		SELECT r.id, r.version_no, d.title
		FROM approval_rounds r
		JOIN documents d ON d.id = r.document_id
		WHERE r.document_id = $1 AND r.status = 'active'
		FOR UPDATE OF r
	`, documentID).Scan(&roundID, &versionNo, &documentTitle)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusConflict, "Активное согласование не найдено")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось найти согласование")
		return
	}
	var pending int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM approval_participants WHERE round_id = $1 AND decision IS NULL`, roundID).Scan(&pending); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось проверить ответы")
		return
	}
	if pending > 0 {
		app.renderDocument(c, http.StatusConflict, documentID, fmt.Sprintf("Нельзя завершить: ожидается ответов — %d", pending))
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE approval_rounds
		SET status = 'completed', final_outcome = $2, final_comment = $3, completed_at = NOW()
		WHERE id = $1
	`, roundID, outcome, comment); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось записать итог")
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE documents SET status = $2, updated_at = NOW() WHERE id = $1`, documentID, outcome); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось изменить статус документа")
		return
	}
	if err := app.writeAudit(ctx, tx, c.MustGet("user").(user), auditRecord{
		EventType: "approval.completed", TargetType: "approval_round", TargetID: &roundID,
		TargetLabel: documentTitle, DocumentID: &documentID, VersionNo: &versionNo,
		Details: approvalOutcomeLabel(outcome),
	}); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось записать событие аудита")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось завершить согласование")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/documents/%d", documentID))
}

func (app *application) cancelApproval(c *gin.Context) {
	documentID, ok := approvalDocumentID(c)
	if !ok {
		return
	}
	comment := strings.TrimSpace(c.PostForm("cancel_comment"))
	if len([]rune(comment)) > 5000 {
		app.renderDocument(c, http.StatusUnprocessableEntity, documentID, "Причина отмены не должна превышать 5000 знаков")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	tx, err := app.db.Begin(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось отменить согласование")
		return
	}
	defer tx.Rollback(ctx)
	var documentTitle string
	if err := tx.QueryRow(ctx, `SELECT title FROM documents WHERE id = $1 FOR UPDATE`, documentID).Scan(&documentTitle); err != nil {
		c.String(http.StatusConflict, "Документ не найден")
		return
	}
	var roundID int64
	var versionNo int
	err = tx.QueryRow(ctx, `
		UPDATE approval_rounds
		SET status = 'cancelled', final_comment = $2, completed_at = NOW()
		WHERE document_id = $1 AND status = 'active'
		RETURNING id, version_no
	`, documentID, comment).Scan(&roundID, &versionNo)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusConflict, "Активное согласование не найдено")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось отменить согласование")
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE documents SET status = 'draft', updated_at = NOW() WHERE id = $1`, documentID); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось изменить статус документа")
		return
	}
	if err := app.writeAudit(ctx, tx, c.MustGet("user").(user), auditRecord{
		EventType: "approval.cancelled", TargetType: "approval_round", TargetID: &roundID,
		TargetLabel: documentTitle, DocumentID: &documentID, VersionNo: &versionNo,
	}); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось записать событие аудита")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось отменить согласование")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/documents/%d", documentID))
}

func approvalDocumentID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		c.String(http.StatusBadRequest, "Некорректный идентификатор документа")
		return 0, false
	}
	return id, true
}

func parseParticipantIDs(values []string) ([]int64, error) {
	seen := make(map[int64]bool)
	result := make([]int64, 0, len(values))
	for _, value := range values {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id < 1 {
			return nil, errors.New("invalid participant")
		}
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	return result, nil
}

func validApprovalDecision(role, decision string) bool {
	if role == "committee" {
		return decision == "approve" || decision == "reject" || decision == "abstain"
	}
	if role == "approver" {
		return decision == "approve" || decision == "approve_with_comments" || decision == "reject"
	}
	return false
}

func approvalDecisionLabel(role, decision string) string {
	if decision == "" {
		return "Ожидается"
	}
	if role == "committee" {
		switch decision {
		case "approve":
			return "За"
		case "reject":
			return "Против"
		case "abstain":
			return "Воздержался"
		}
	}
	switch decision {
	case "approve":
		return "Согласовано"
	case "approve_with_comments":
		return "Согласовано с замечаниями"
	case "reject":
		return "Не согласовано"
	default:
		return decision
	}
}

func approvalRoundStatusLabel(status string) string {
	switch status {
	case "active":
		return "Идёт согласование"
	case "completed":
		return "Завершено"
	case "cancelled":
		return "Отменено"
	default:
		return status
	}
}

func approvalOutcomeLabel(outcome string) string {
	if outcome == "approved" {
		return "Согласовано"
	}
	if outcome == "rejected" {
		return "Не согласовано"
	}
	return ""
}
