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

type decisionRevisionInput struct {
	Text, Reason, Policy, Deadline, Token string
}

type decisionRevisionState struct {
	Title, Text, Status, QuestionType string
	UpdatedAt                         time.Time
	SourceRoundID                     int64
}

type decisionRevisionItem struct {
	Number                                                         int
	PreviousText, Text, Reason, PolicyLabel, Creator, CreatedLabel string
	SourceRoundID, NewRoundID                                      int64
}

type decisionRevisionRuleError struct {
	Status  int
	Message string
}

func (e *decisionRevisionRuleError) Error() string { return e.Message }
func revisionRule(status int, message string) error {
	return &decisionRevisionRuleError{status, message}
}

func canReviseDecision(status string, hasHistory bool) bool {
	if !hasHistory {
		return false
	}
	switch status {
	case "draft", "revision_required", "ready_for_committee", "rejected", "no_quorum":
		return true
	default:
		return false
	}
}

func decisionVisaPolicyLabel(policy string) string {
	if policy == "carry_positive" {
		return "Перенос положительных виз по неизменившимся файлам"
	}
	return "Все визы запрашиваются заново"
}

func validateDecisionRevision(input decisionRevisionInput, now time.Time) (time.Time, time.Time, error) {
	if strings.TrimSpace(input.Text) == "" || len([]rune(input.Text)) > 10000 {
		return time.Time{}, time.Time{}, revisionRule(422, "Укажите формулировку решения длиной от 1 до 10 000 знаков")
	}
	if strings.TrimSpace(input.Reason) == "" || len([]rune(input.Reason)) > 2000 {
		return time.Time{}, time.Time{}, revisionRule(422, "Укажите причину изменения длиной от 1 до 2000 знаков")
	}
	if input.Policy != "recheck_all" && input.Policy != "carry_positive" {
		return time.Time{}, time.Time{}, revisionRule(422, "Выберите, как поступить с прежними визами")
	}
	deadline, err := time.Parse("2006-01-02", input.Deadline)
	if err != nil || deadline.Before(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)) {
		return time.Time{}, time.Time{}, revisionRule(422, "Укажите срок внутреннего согласования не раньше сегодняшнего дня")
	}
	expected, err := time.Parse(time.RFC3339Nano, input.Token)
	if err != nil {
		return time.Time{}, time.Time{}, revisionRule(409, errQuestionEditConflict.Error())
	}
	return deadline, expected, nil
}

func loadDecisionRevisionState(ctx context.Context, db questionEditQueryer, id int64, lock bool) (decisionRevisionState, error) {
	var state decisionRevisionState
	sql := `SELECT title, decision_text, status, question_type, updated_at FROM questions WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	if err := db.QueryRow(ctx, sql, id).Scan(&state.Title, &state.Text, &state.Status, &state.QuestionType, &state.UpdatedAt); err != nil {
		return state, err
	}
	var active bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM internal_review_rounds WHERE question_id = $1 AND status = 'active')
		OR EXISTS (SELECT 1 FROM committee_vote_rounds WHERE question_id = $1 AND status = 'active')`, id).Scan(&active); err != nil {
		return state, err
	}
	err := db.QueryRow(ctx, `SELECT id FROM internal_review_rounds WHERE question_id = $1 ORDER BY started_at DESC, id DESC LIMIT 1`, id).Scan(&state.SourceRoundID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return state, err
	}
	if active || !canReviseDecision(state.Status, state.SourceRoundID > 0) {
		return state, revisionRule(409, "Новая редакция доступна после завершения или отмены раунда, до положительного решения. Для активного раунда сначала выполните отмену")
	}
	return state, nil
}

func (app *application) renderDecisionRevision(c *gin.Context, code int, id int64, input decisionRevisionInput, message string) {
	c.HTML(code, "decision-revision.html", gin.H{"Title": "Новая редакция решения", "User": c.MustGet("user").(user),
		"QuestionID": id, "Input": input, "Error": message, "CSRFToken": app.templateCSRF(c)})
}

func (app *application) showDecisionRevision(c *gin.Context) {
	id, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	state, err := loadDecisionRevisionState(c.Request.Context(), app.db, id, false)
	if err != nil {
		var rule *decisionRevisionRuleError
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			c.String(404, "Вопрос не найден")
		case errors.As(err, &rule):
			c.String(rule.Status, rule.Message)
		default:
			c.String(500, "Не удалось загрузить вопрос")
		}
		return
	}
	app.renderDecisionRevision(c, 200, id, decisionRevisionInput{Text: state.Text, Token: state.UpdatedAt.Format(time.RFC3339Nano), Deadline: time.Now().AddDate(0, 0, 7).Format("2006-01-02")}, "")
}

func (app *application) createDecisionRevision(c *gin.Context) {
	id, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	input := decisionRevisionInput{Text: strings.TrimSpace(c.PostForm("decision_text")), Reason: strings.TrimSpace(c.PostForm("reason")),
		Policy: c.PostForm("visa_policy"), Deadline: c.PostForm("deadline"), Token: c.PostForm("edit_token")}
	_, _, err := validateDecisionRevision(input, time.Now())
	if err == nil {
		var tx pgx.Tx
		tx, err = app.db.Begin(c.Request.Context())
		if err == nil {
			defer tx.Rollback(c.Request.Context())
			err = lockUserAssignments(c, tx)
			if err == nil {
				err = app.createDecisionRevisionTransaction(c.Request.Context(), tx, c.MustGet("user").(user), id, input, time.Now())
			}
		}
	}
	if err == nil {
		c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", id))
		return
	}
	var rule *decisionRevisionRuleError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		c.String(404, "Вопрос не найден")
	case errors.As(err, &rule):
		app.renderDecisionRevision(c, rule.Status, id, input, rule.Message)
	default:
		app.renderDecisionRevision(c, 500, id, input, "Не удалось сохранить редакцию решения")
	}
}

// Only positive visas for the same file version may cross a text revision.
// A rejected file still needs a new official version, regardless of the policy.
func planDecisionRevision(files []revisionFile, sources map[string]revisionSourceVisa, policy string) (map[string]revisionSourceVisa, error) {
	carry := make(map[string]revisionSourceVisa)
	for _, file := range files {
		for _, service := range requiredInternalServices {
			key := revisionVisaKey(file.ID, service)
			source, found := sources[key]
			if !found {
				continue
			}
			if source.Decision == "rejected" && source.VersionNo == file.CurrentVersion {
				return nil, revisionRule(422, "Сначала загрузите и подтвердите новую версию каждого несогласованного файла")
			}
			if policy == "carry_positive" && source.VersionNo == file.CurrentVersion &&
				(source.Decision == "approved" || source.Decision == "approved_with_comments" || source.Decision == "no_comments_without_review") {
				carry[key] = source
			}
		}
	}
	return carry, nil
}

func (app *application) createDecisionRevisionTransaction(ctx context.Context, tx pgx.Tx, actor user, id int64, input decisionRevisionInput, now time.Time) error {
	defer tx.Rollback(ctx)
	deadline, expected, err := validateDecisionRevision(input, now)
	if err != nil {
		return err
	}
	state, err := loadDecisionRevisionState(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if !state.UpdatedAt.Equal(expected) {
		return revisionRule(409, errQuestionEditConflict.Error())
	}
	if input.Text == state.Text {
		return revisionRule(422, "Новая формулировка совпадает с текущей")
	}
	files, err := loadRevisionFiles(ctx, tx, id)
	if err != nil {
		return err
	}
	sources, err := loadRevisionSourceVisas(ctx, tx, state.SourceRoundID)
	if err != nil {
		return err
	}
	carry, err := planDecisionRevision(files, sources, input.Policy)
	if err != nil {
		return err
	}
	services, err := loadActiveServiceCounts(ctx, tx)
	if err != nil {
		return err
	}
	var pending int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM question_file_versions v JOIN question_files f ON f.id = v.question_file_id
		WHERE f.question_id = $1 AND f.status = 'active' AND v.approval_status = 'pending'`, id).Scan(&pending); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id, current_version_no, category FROM question_files
		WHERE question_id = $1 AND status = 'active' AND current_version_no IS NOT NULL ORDER BY id`, id)
	if err != nil {
		return err
	}
	var startFiles []reviewStartFile
	for rows.Next() {
		var file reviewStartFile
		if err := rows.Scan(&file.ID, &file.VersionNo, &file.Category); err != nil {
			rows.Close()
			return err
		}
		startFiles = append(startFiles, file)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if err := validateInternalReviewStart(state.QuestionType, deadline, now, startFiles, services, pending); err != nil {
		return revisionRule(422, err.Error())
	}
	var roundID int64
	if err := tx.QueryRow(ctx, `INSERT INTO internal_review_rounds (question_id, deadline, started_by, frozen_decision_text)
		VALUES ($1, $2, $3, $4) RETURNING id`, id, deadline, actor.ID, input.Text).Scan(&roundID); err != nil {
		return err
	}
	for _, file := range files {
		for _, service := range requiredInternalServices {
			source, carried := carry[revisionVisaKey(file.ID, service)]
			status := "pending"
			if carried {
				status = "responded"
			}
			var requirementID int64
			if err := tx.QueryRow(ctx, `INSERT INTO internal_review_requirements (round_id, question_file_id, version_no, internal_service, status)
				VALUES ($1, $2, $3, $4, $5) RETURNING id`, roundID, file.ID, file.CurrentVersion, service, status).Scan(&requirementID); err != nil {
				return err
			}
			if carried {
				if _, err := tx.Exec(ctx, `INSERT INTO internal_review_visas
					(requirement_id, decision, comment, decided_by, decided_at, source_visa_id, carried_by, carried_at)
					VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())`, requirementID, source.Decision, source.Comment,
					source.DecidedBy, source.DecidedAt, source.VisaID, actor.ID); err != nil {
					return err
				}
			}
		}
	}
	var revisionNo int
	if err := tx.QueryRow(ctx, `INSERT INTO decision_text_revisions
		(question_id, revision_no, previous_text, decision_text, reason, visa_policy, source_round_id, new_round_id, created_by, creator_name)
		SELECT $1, COALESCE(MAX(revision_no), 0) + 1, $2, $3, $4, $5, $6, $7, $8, $9
		FROM decision_text_revisions WHERE question_id = $1 RETURNING revision_no`,
		id, state.Text, input.Text, input.Reason, input.Policy, state.SourceRoundID, roundID, actor.ID, actor.FullName).Scan(&revisionNo); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE questions SET decision_text = $2, status = 'internal_review', internal_deadline = $3,
		updated_at = clock_timestamp() WHERE id = $1`, id, input.Text, deadline); err != nil {
		return err
	}
	if err := app.writeAudit(ctx, tx, actor, auditRecord{EventType: "question.decision_revised", TargetType: "question", TargetID: &id,
		TargetLabel: state.Title, QuestionID: &id, Details: fmt.Sprintf("Редакция №%d, раунд №%d после №%d. %s. Перенесено виз: %d; новых: %d. Причина: %s",
			revisionNo, roundID, state.SourceRoundID, decisionVisaPolicyLabel(input.Policy), len(carry), len(files)*4-len(carry), input.Reason)}); err != nil {
		return err
	}
	if err := app.writeAudit(ctx, tx, actor, auditRecord{EventType: "internal_review.started", TargetType: "question", TargetID: &id,
		TargetLabel: state.Title, QuestionID: &id, Details: fmt.Sprintf("Раунд №%d для редакции решения №%d", roundID, revisionNo)}); err != nil {
		return err
	}
	if err := app.finishInternalReviewIfComplete(ctx, tx, actor, id, roundID, state.Title); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (app *application) loadDecisionRevisions(ctx context.Context, id int64) ([]decisionRevisionItem, error) {
	rows, err := app.db.Query(ctx, `SELECT revision_no, previous_text, decision_text, reason, visa_policy, creator_name,
		created_at, source_round_id, new_round_id FROM decision_text_revisions WHERE question_id = $1 ORDER BY revision_no DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []decisionRevisionItem
	for rows.Next() {
		var item decisionRevisionItem
		var created time.Time
		var policy string
		if err := rows.Scan(&item.Number, &item.PreviousText, &item.Text, &item.Reason, &policy, &item.Creator, &created, &item.SourceRoundID, &item.NewRoundID); err != nil {
			return nil, err
		}
		item.PolicyLabel, item.CreatedLabel = decisionVisaPolicyLabel(policy), created.Format("02.01.2006 15:04")
		items = append(items, item)
	}
	return items, rows.Err()
}
