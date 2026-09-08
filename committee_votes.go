package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

type committeeVoteView struct {
	HasRound      bool
	Active        bool
	CanStart      bool
	CanManage     bool
	CanClose      bool
	CanVote       bool
	StatusLabel   string
	DecisionText  string
	DeadlineLabel string
	DeadlineValue string
	ProgressLabel string
	QuorumLabel   string
	PastDue       bool
	ParticipantID int64
	Files         []committeeFrozenFile
	Participants  []committeeParticipantView
}

type committeeFrozenFile struct {
	ID        int64
	Title     string
	VersionNo int
}

type committeeParticipantView struct {
	Name    string
	IsChair bool
	HasVote bool
	CanVote bool
	Vote    committeeVoteItem
	History []committeeVoteItem
}

type committeeVoteItem struct {
	ID          int64
	Decision    string
	Comment     string
	TargetLabel string
	VotedLabel  string
	Withdrawn   bool
	CanWithdraw bool
	Attachments []committeeAttachmentItem
}

func validCommitteeDecision(value string) bool {
	switch value {
	case "for", "for_with_comments", "against", "abstain":
		return true
	default:
		return false
	}
}

func validateCommitteeDecision(decision, comment string) error {
	if !validCommitteeDecision(decision) {
		return errors.New("Выберите допустимый вариант голоса")
	}
	if len([]rune(comment)) > 5000 {
		return errors.New("Комментарий не должен превышать 5 000 знаков")
	}
	if decision != "for" && comment == "" {
		return errors.New("Для выбранного варианта укажите комментарий")
	}
	return nil
}

func committeeDecisionLabel(value string) string {
	labels := map[string]string{
		"for": "За", "for_with_comments": "За с замечаниями",
		"against": "Против", "abstain": "Воздержался",
	}
	return labels[value]
}

func calculateCommitteeOutcome(roster, responded, support int) (bool, string) {
	if roster < 1 || responded*2 <= roster {
		return false, "no_quorum"
	}
	if support*2 > responded {
		return true, "approved"
	}
	return true, "rejected"
}

func (app *application) startCommitteeVote(c *gin.Context) {
	usr := c.MustGet("user").(user)
	questionID, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	deadline, err := time.Parse("2006-01-02", strings.TrimSpace(c.PostForm("deadline")))
	if err != nil {
		c.String(http.StatusUnprocessableEntity, "Укажите срок голосования")
		return
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if deadline.Before(today) {
		c.String(http.StatusUnprocessableEntity, "Срок голосования не может быть в прошлом")
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать передачу на Комитет")
		return
	}
	defer tx.Rollback(c.Request.Context())
	if err := lockUserAssignments(c, tx); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось зафиксировать состав Комитета")
		return
	}
	var title, decisionText, status string
	err = tx.QueryRow(c.Request.Context(), `
		SELECT title, decision_text, status FROM questions WHERE id = $1 FOR UPDATE
	`, questionID).Scan(&title, &decisionText, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Вопрос не найден")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить вопрос")
		return
	}
	if status != "ready_for_committee" && status != "rejected" && status != "no_quorum" {
		c.String(http.StatusConflict, "Вопрос пока нельзя передать на Комитет")
		return
	}
	fileRows, err := tx.Query(c.Request.Context(), `
		SELECT id, title, current_version_no FROM question_files
		WHERE question_id = $1 AND status = 'active' AND current_version_no IS NOT NULL
		ORDER BY created_at, id FOR UPDATE
	`, questionID)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось зафиксировать комплект")
		return
	}
	var files []committeeFrozenFile
	for fileRows.Next() {
		var file committeeFrozenFile
		if err := fileRows.Scan(&file.ID, &file.Title, &file.VersionNo); err != nil {
			fileRows.Close()
			c.String(http.StatusInternalServerError, "Не удалось прочитать комплект")
			return
		}
		files = append(files, file)
	}
	if err := fileRows.Err(); err != nil {
		fileRows.Close()
		c.String(http.StatusInternalServerError, "Не удалось прочитать комплект")
		return
	}
	fileRows.Close()
	if len(files) == 0 {
		c.String(http.StatusUnprocessableEntity, "Комплект вопроса пуст")
		return
	}
	memberRows, err := tx.Query(c.Request.Context(), `
		SELECT id, full_name, email, is_committee_chair FROM users
		WHERE active = TRUE AND role = 'committee'
		ORDER BY full_name, id FOR UPDATE
	`)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить состав Комитета")
		return
	}
	type member struct {
		ID      int64
		Name    string
		Email   string
		IsChair bool
	}
	var members []member
	chairCount := 0
	for memberRows.Next() {
		var item member
		if err := memberRows.Scan(&item.ID, &item.Name, &item.Email, &item.IsChair); err != nil {
			memberRows.Close()
			c.String(http.StatusInternalServerError, "Не удалось прочитать состав Комитета")
			return
		}
		if item.IsChair {
			chairCount++
		}
		members = append(members, item)
	}
	if err := memberRows.Err(); err != nil {
		memberRows.Close()
		c.String(http.StatusInternalServerError, "Не удалось прочитать состав Комитета")
		return
	}
	memberRows.Close()
	if len(members) == 0 || chairCount != 1 {
		c.String(http.StatusUnprocessableEntity, "Для запуска нужны активные члены Комитета и ровно один председатель")
		return
	}
	var roundID int64
	err = tx.QueryRow(c.Request.Context(), `
		INSERT INTO committee_vote_rounds (
			question_id, deadline, frozen_decision_text, roster_size, started_by
		) VALUES ($1, $2, $3, $4, $5) RETURNING id
	`, questionID, deadline, decisionText, len(members), usr.ID).Scan(&roundID)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось создать голосование")
		return
	}
	for _, file := range files {
		if _, err := tx.Exec(c.Request.Context(), `
			INSERT INTO committee_vote_round_files (round_id, question_file_id, version_no)
			VALUES ($1, $2, $3)
		`, roundID, file.ID, file.VersionNo); err != nil {
			c.String(http.StatusInternalServerError, "Не удалось зафиксировать версии файлов")
			return
		}
	}
	for _, member := range members {
		if _, err := tx.Exec(c.Request.Context(), `
			INSERT INTO committee_vote_participants (
				round_id, user_id, name_snapshot, email_snapshot, is_chair_snapshot
			) VALUES ($1, $2, $3, $4, $5)
		`, roundID, member.ID, member.Name, member.Email, member.IsChair); err != nil {
			c.String(http.StatusInternalServerError, "Не удалось зафиксировать состав Комитета")
			return
		}
	}
	_, err = tx.Exec(c.Request.Context(), `
		UPDATE questions SET status = 'committee_voting', updated_at = NOW() WHERE id = $1
	`, questionID)
	if err == nil {
		err = app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
			EventType: "committee_vote.started", TargetType: "question", TargetID: &questionID,
			TargetLabel: title, QuestionID: &questionID,
			Details: fmt.Sprintf("Раунд №%d, участников: %d, файлов: %d, срок: %s", roundID, len(members), len(files), deadline.Format("02.01.2006")),
		})
	}
	if err == nil {
		err = tx.Commit(c.Request.Context())
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось передать вопрос на Комитет")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", questionID))
}

func (app *application) submitCommitteeVote(c *gin.Context) {
	usr := c.MustGet("user").(user)
	questionID, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxDocumentSize*maxVisaAttachments+(2<<20))
	decision := strings.TrimSpace(c.PostForm("decision"))
	comment := strings.TrimSpace(c.PostForm("comment"))
	if err := validateCommitteeDecision(decision, comment); err != nil {
		c.String(http.StatusUnprocessableEntity, err.Error())
		return
	}
	var targetFileID int64
	if value := strings.TrimSpace(c.PostForm("target_file_id")); value != "" {
		var err error
		targetFileID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || targetFileID < 1 {
			c.String(http.StatusBadRequest, "Некорректная привязка комментария")
			return
		}
	}
	attachments, attachmentStatus, attachmentMessage := receiveVisaAttachments(c)
	if attachmentMessage != "" {
		c.String(attachmentStatus, attachmentMessage)
		return
	}
	defer closeQuestionUploads(attachments)
	if len(attachments) > 0 && app.storage == nil {
		c.String(http.StatusServiceUnavailable, "S3 не настроен: вложения не сохранены")
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать сохранение голоса")
		return
	}
	defer tx.Rollback(c.Request.Context())
	var storedAttachments []storedS3Object
	committed := false
	defer func() {
		if !committed {
			app.removeStoredS3Objects(storedAttachments)
		}
	}()
	var title, status string
	err = tx.QueryRow(c.Request.Context(), `SELECT title, status FROM questions WHERE id = $1 FOR UPDATE`, questionID).Scan(&title, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Вопрос не найден")
		return
	}
	if err != nil || status != "committee_voting" {
		c.String(http.StatusConflict, "Голосование вопроса не активно")
		return
	}
	var roundID, participantID int64
	var participantStatus string
	var deadline time.Time
	err = tx.QueryRow(c.Request.Context(), `
		SELECT participant.round_id, participant.id, participant.status, round.deadline
		FROM committee_vote_participants participant
		JOIN committee_vote_rounds round ON round.id = participant.round_id
		WHERE round.question_id = $1 AND round.status = 'active' AND participant.user_id = $2
		FOR UPDATE OF participant, round
	`, questionID, usr.ID).Scan(&roundID, &participantID, &participantStatus, &deadline)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusForbidden, "Вы не входите в зафиксированный состав этого голосования")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить участие в голосовании")
		return
	}
	if participantStatus != "pending" {
		c.String(http.StatusConflict, "Действующий голос уже отправлен")
		return
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if deadline.Before(today) {
		c.String(http.StatusConflict, "Срок голосования истёк: секретарь должен продлить или закрыть раунд")
		return
	}
	var targetVersionNo int
	if targetFileID > 0 {
		if err := tx.QueryRow(c.Request.Context(), `
			SELECT version_no FROM committee_vote_round_files WHERE round_id = $1 AND question_file_id = $2
		`, roundID, targetFileID).Scan(&targetVersionNo); errors.Is(err, pgx.ErrNoRows) {
			c.String(http.StatusUnprocessableEntity, "Выбранный файл не входит в замороженный комплект")
			return
		} else if err != nil {
			c.String(http.StatusInternalServerError, "Не удалось проверить привязку комментария")
			return
		}
	}
	var voteID int64
	var target, targetVersion any
	if targetFileID > 0 {
		target = targetFileID
		targetVersion = targetVersionNo
	}
	err = tx.QueryRow(c.Request.Context(), `
		INSERT INTO committee_votes (participant_id, decision, comment, target_question_file_id, target_version_no)
		VALUES ($1, $2, $3, $4, $5) RETURNING id
	`, participantID, decision, comment, target, targetVersion).Scan(&voteID)
	if err == nil && len(attachments) > 0 {
		storedAttachments, err = app.storeCommitteeAttachments(c.Request.Context(), tx, questionID, voteID, usr, attachments)
	}
	if err == nil {
		_, err = tx.Exec(c.Request.Context(), `UPDATE committee_vote_participants SET status = 'voted' WHERE id = $1`, participantID)
	}
	if err == nil {
		err = app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
			EventType: "committee_vote.submitted", TargetType: "committee_vote", TargetID: &voteID,
			TargetLabel: title, QuestionID: &questionID,
			Details: fmt.Sprintf("Голос: %s. Вложений: %d", committeeDecisionLabel(decision), len(attachments)),
		})
	}
	if err == nil {
		err = app.finishCommitteeVoteIfEveryoneResponded(c.Request.Context(), tx, usr, questionID, roundID, title)
	}
	if err == nil {
		err = tx.Commit(c.Request.Context())
	}
	if err != nil {
		log.Printf("save committee vote for question %d: %v", questionID, err)
		c.String(http.StatusInternalServerError, "Не удалось сохранить голос")
		return
	}
	committed = true
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", questionID))
}

func (app *application) finishCommitteeVoteIfEveryoneResponded(ctx context.Context, tx pgx.Tx, actor user, questionID, roundID int64, title string) error {
	var roster, responded, support int
	err := tx.QueryRow(ctx, `
		SELECT round.roster_size,
		       COUNT(*) FILTER (WHERE participant.status = 'voted'),
		       COUNT(*) FILTER (WHERE vote.decision IN ('for', 'for_with_comments'))
		FROM committee_vote_rounds round
		JOIN committee_vote_participants participant ON participant.round_id = round.id
		LEFT JOIN committee_votes vote ON vote.participant_id = participant.id AND vote.withdrawn_at IS NULL
		WHERE round.id = $1 GROUP BY round.roster_size
	`, roundID).Scan(&roster, &responded, &support)
	if err != nil || responded < roster {
		return err
	}
	_, outcome := calculateCommitteeOutcome(roster, responded, support)
	return app.completeCommitteeVote(ctx, tx, actor, questionID, roundID, title, outcome, roster, responded, support, "Все участники проголосовали")
}

func (app *application) completeCommitteeVote(ctx context.Context, tx pgx.Tx, actor user, questionID, roundID int64, title, outcome string, roster, responded, support int, reason string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE committee_vote_rounds SET status = 'completed', outcome = $2, completed_at = NOW()
		WHERE id = $1 AND status = 'active'
	`, roundID, outcome); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE questions SET status = $2, updated_at = NOW() WHERE id = $1`, questionID, outcome); err != nil {
		return err
	}
	return app.writeAudit(ctx, tx, actor, auditRecord{
		EventType: "committee_vote.completed", TargetType: "question", TargetID: &questionID,
		TargetLabel: title, QuestionID: &questionID,
		Details: fmt.Sprintf("%s. Состав: %d, проголосовали: %d, голоса за: %d. Итог: %s", reason, roster, responded, support, questionStatusLabel(outcome)),
	})
}

func (app *application) withdrawCommitteeVote(c *gin.Context) {
	usr := c.MustGet("user").(user)
	questionID, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	voteID, ok := parsePositiveID(c, "voteID", "Некорректный идентификатор голоса")
	if !ok {
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать отзыв голоса")
		return
	}
	defer tx.Rollback(c.Request.Context())
	var participantID int64
	var votedAt time.Time
	var title string
	err = tx.QueryRow(c.Request.Context(), `
		SELECT participant.id, vote.voted_at, question.title
		FROM committee_votes vote
		JOIN committee_vote_participants participant ON participant.id = vote.participant_id
		JOIN committee_vote_rounds round ON round.id = participant.round_id
		JOIN questions question ON question.id = round.question_id
		WHERE vote.id = $1 AND question.id = $2 AND participant.user_id = $3
		  AND vote.withdrawn_at IS NULL AND round.status = 'active'
		FOR UPDATE OF vote, participant, round, question
	`, voteID, questionID, usr.ID).Scan(&participantID, &votedAt, &title)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Действующий голос не найден")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить голос")
		return
	}
	if time.Now().After(votedAt.Add(24 * time.Hour)) {
		c.String(http.StatusConflict, "С момента голосования прошло более 24 часов")
		return
	}
	_, err = tx.Exec(c.Request.Context(), `UPDATE committee_votes SET withdrawn_at = NOW() WHERE id = $1`, voteID)
	if err == nil {
		_, err = tx.Exec(c.Request.Context(), `UPDATE committee_vote_participants SET status = 'pending' WHERE id = $1`, participantID)
	}
	if err == nil {
		err = app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
			EventType: "committee_vote.withdrawn", TargetType: "committee_vote", TargetID: &voteID,
			TargetLabel: title, QuestionID: &questionID,
		})
	}
	if err == nil {
		err = tx.Commit(c.Request.Context())
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось отозвать голос")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", questionID))
}

func (app *application) extendCommitteeVote(c *gin.Context) {
	usr := c.MustGet("user").(user)
	questionID, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	deadline, err := time.Parse("2006-01-02", strings.TrimSpace(c.PostForm("deadline")))
	reason := strings.TrimSpace(c.PostForm("reason"))
	if err != nil || reason == "" || len([]rune(reason)) > 2000 {
		c.String(http.StatusUnprocessableEntity, "Укажите новый срок и причину длиной до 2 000 знаков")
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать продление голосования")
		return
	}
	defer tx.Rollback(c.Request.Context())
	var roundID int64
	var oldDeadline time.Time
	var title string
	err = tx.QueryRow(c.Request.Context(), `
		SELECT round.id, round.deadline, question.title
		FROM committee_vote_rounds round JOIN questions question ON question.id = round.question_id
		WHERE round.question_id = $1 AND round.status = 'active' FOR UPDATE OF round, question
	`, questionID).Scan(&roundID, &oldDeadline, &title)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusConflict, "Активное голосование не найдено")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить голосование")
		return
	}
	if !deadline.After(oldDeadline) {
		c.String(http.StatusUnprocessableEntity, "Новый срок должен быть позже текущего")
		return
	}
	_, err = tx.Exec(c.Request.Context(), `UPDATE committee_vote_rounds SET deadline = $2 WHERE id = $1`, roundID, deadline)
	if err == nil {
		err = app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
			EventType: "committee_vote.extended", TargetType: "question", TargetID: &questionID,
			TargetLabel: title, QuestionID: &questionID,
			Details: fmt.Sprintf("Срок изменён с %s на %s. Причина: %s", oldDeadline.Format("02.01.2006"), deadline.Format("02.01.2006"), reason),
		})
	}
	if err == nil {
		err = tx.Commit(c.Request.Context())
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось продлить голосование")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", questionID))
}

func (app *application) closeCommitteeVote(c *gin.Context) {
	usr := c.MustGet("user").(user)
	questionID, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать закрытие голосования")
		return
	}
	defer tx.Rollback(c.Request.Context())
	var roundID int64
	var roster int
	var deadline time.Time
	var title string
	err = tx.QueryRow(c.Request.Context(), `
		SELECT round.id, round.roster_size, round.deadline, question.title
		FROM committee_vote_rounds round JOIN questions question ON question.id = round.question_id
		WHERE round.question_id = $1 AND round.status = 'active' FOR UPDATE OF round, question
	`, questionID).Scan(&roundID, &roster, &deadline, &title)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusConflict, "Активное голосование не найдено")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить голосование")
		return
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if !deadline.Before(today) {
		c.String(http.StatusConflict, "Закрыть голосование вручную можно после истечения срока")
		return
	}
	var responded, support int
	err = tx.QueryRow(c.Request.Context(), `
		SELECT COUNT(*) FILTER (WHERE participant.status = 'voted'),
		       COUNT(*) FILTER (WHERE vote.decision IN ('for', 'for_with_comments'))
		FROM committee_vote_participants participant
		LEFT JOIN committee_votes vote ON vote.participant_id = participant.id AND vote.withdrawn_at IS NULL
		WHERE participant.round_id = $1
	`, roundID).Scan(&responded, &support)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось подсчитать голоса")
		return
	}
	_, outcome := calculateCommitteeOutcome(roster, responded, support)
	err = app.completeCommitteeVote(c.Request.Context(), tx, usr, questionID, roundID, title, outcome, roster, responded, support, "Голосование закрыто секретарём после срока")
	if err == nil {
		err = tx.Commit(c.Request.Context())
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось закрыть голосование")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", questionID))
}

func (app *application) cancelCommitteeVote(c *gin.Context) {
	usr := c.MustGet("user").(user)
	questionID, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать отмену голосования")
		return
	}
	defer tx.Rollback(c.Request.Context())
	var roundID int64
	var title string
	err = tx.QueryRow(c.Request.Context(), `
		SELECT round.id, question.title FROM committee_vote_rounds round
		JOIN questions question ON question.id = round.question_id
		WHERE round.question_id = $1 AND round.status = 'active' FOR UPDATE OF round, question
	`, questionID).Scan(&roundID, &title)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusConflict, "Активное голосование не найдено")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить голосование")
		return
	}
	_, err = tx.Exec(c.Request.Context(), `UPDATE committee_vote_rounds SET status = 'cancelled', cancelled_at = NOW() WHERE id = $1`, roundID)
	if err == nil {
		_, err = tx.Exec(c.Request.Context(), `UPDATE questions SET status = 'ready_for_committee', updated_at = NOW() WHERE id = $1`, questionID)
	}
	if err == nil {
		err = app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
			EventType: "committee_vote.cancelled", TargetType: "question", TargetID: &questionID,
			TargetLabel: title, QuestionID: &questionID, Details: fmt.Sprintf("Отменён раунд №%d", roundID),
		})
	}
	if err == nil {
		err = tx.Commit(c.Request.Context())
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось отменить голосование")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", questionID))
}

func (app *application) loadCommitteeVote(ctx context.Context, questionID int64, questionStatus string, usr user) (committeeVoteView, error) {
	view := committeeVoteView{
		CanStart:      usr.Role == "secretary" && (questionStatus == "ready_for_committee" || questionStatus == "rejected" || questionStatus == "no_quorum"),
		DeadlineValue: time.Now().AddDate(0, 0, 7).Format("2006-01-02"),
	}
	var roundID int64
	var status, outcome string
	var deadline time.Time
	var roster int
	err := app.db.QueryRow(ctx, `
		SELECT id, status, COALESCE(outcome, ''), frozen_decision_text, deadline, roster_size
		FROM committee_vote_rounds WHERE question_id = $1
		ORDER BY started_at DESC, id DESC LIMIT 1
	`, questionID).Scan(&roundID, &status, &outcome, &view.DecisionText, &deadline, &roster)
	if errors.Is(err, pgx.ErrNoRows) {
		return view, nil
	}
	if err != nil {
		return committeeVoteView{}, err
	}
	view.HasRound = true
	view.Active = status == "active"
	view.CanManage = view.Active && usr.Role == "secretary"
	view.DeadlineLabel = deadline.Format("02.01.2006")
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	view.PastDue = view.Active && deadline.Before(today)
	view.CanClose = view.CanManage && view.PastDue
	switch status {
	case "active":
		view.StatusLabel = "Идёт голосование"
	case "cancelled":
		view.StatusLabel = "Голосование отменено"
	default:
		view.StatusLabel = questionStatusLabel(outcome)
	}
	fileRows, err := app.db.Query(ctx, `
		SELECT frozen.question_file_id, file.title, frozen.version_no
		FROM committee_vote_round_files frozen
		JOIN question_files file ON file.id = frozen.question_file_id
		WHERE frozen.round_id = $1 ORDER BY file.created_at, file.id
	`, roundID)
	if err != nil {
		return committeeVoteView{}, err
	}
	for fileRows.Next() {
		var file committeeFrozenFile
		if err := fileRows.Scan(&file.ID, &file.Title, &file.VersionNo); err != nil {
			fileRows.Close()
			return committeeVoteView{}, err
		}
		view.Files = append(view.Files, file)
	}
	if err := fileRows.Err(); err != nil {
		fileRows.Close()
		return committeeVoteView{}, err
	}
	fileRows.Close()
	attachments, err := loadCommitteeAttachmentViews(ctx, app.db, roundID)
	if err != nil {
		return committeeVoteView{}, err
	}
	rows, err := app.db.Query(ctx, `
		SELECT participant.id, participant.user_id, participant.name_snapshot,
		       participant.is_chair_snapshot, participant.status,
		       COALESCE(vote.id, 0), COALESCE(vote.decision, ''), COALESCE(vote.comment, ''),
		       COALESCE(file.title || ' · версия ' || vote.target_version_no::TEXT, ''),
		       COALESCE(vote.voted_at, 'epoch'::timestamptz),
		       vote.withdrawn_at IS NOT NULL
		FROM committee_vote_participants participant
		LEFT JOIN committee_votes vote ON vote.participant_id = participant.id
		LEFT JOIN question_files file ON file.id = vote.target_question_file_id
		WHERE participant.round_id = $1
		ORDER BY participant.name_snapshot, participant.id, vote.voted_at DESC, vote.id DESC
	`, roundID)
	if err != nil {
		return committeeVoteView{}, err
	}
	defer rows.Close()
	indexes := make(map[int64]int)
	responded := 0
	support := 0
	for rows.Next() {
		var participantID, participantUserID, voteID int64
		var name, participantStatus, decision, comment, targetLabel string
		var isChair, withdrawn bool
		var votedAt time.Time
		if err := rows.Scan(&participantID, &participantUserID, &name, &isChair, &participantStatus,
			&voteID, &decision, &comment, &targetLabel, &votedAt, &withdrawn); err != nil {
			return committeeVoteView{}, err
		}
		index, exists := indexes[participantID]
		if !exists {
			index = len(view.Participants)
			indexes[participantID] = index
			view.Participants = append(view.Participants, committeeParticipantView{Name: name, IsChair: isChair})
			if participantStatus == "voted" {
				responded++
			}
			if participantUserID == usr.ID && view.Active && !view.PastDue && participantStatus == "pending" {
				view.Participants[index].CanVote = true
				view.CanVote = true
				view.ParticipantID = participantID
			}
		}
		if voteID == 0 {
			continue
		}
		vote := committeeVoteItem{
			ID: voteID, Decision: committeeDecisionLabel(decision), Comment: comment,
			TargetLabel: targetLabel, VotedLabel: votedAt.Format("02.01.2006 15:04"),
			Withdrawn: withdrawn, Attachments: attachments[voteID],
		}
		if withdrawn {
			view.Participants[index].History = append(view.Participants[index].History, vote)
			continue
		}
		if decision == "for" || decision == "for_with_comments" {
			support++
		}
		vote.CanWithdraw = view.Active && participantUserID == usr.ID && !time.Now().After(votedAt.Add(24*time.Hour))
		view.Participants[index].HasVote = true
		view.Participants[index].Vote = vote
	}
	view.ProgressLabel = fmt.Sprintf("%d из %d", responded, roster)
	hasQuorum, _ := calculateCommitteeOutcome(roster, responded, support)
	if hasQuorum {
		view.QuorumLabel = "Кворум набран"
	} else {
		view.QuorumLabel = "Кворума пока нет"
	}
	return view, rows.Err()
}
