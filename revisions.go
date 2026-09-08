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

type revisionPlanView struct {
	Available     bool
	CanRestart    bool
	Ready         bool
	DeadlineValue string
	BlockReason   string
	Files         []revisionFileView
}

type revisionFileView struct {
	ID              int64
	Title           string
	PreviousVersion int
	CurrentVersion  int
	Changed         bool
	IsNew           bool
	Services        []revisionServiceView
}

type revisionServiceView struct {
	Value         string
	ServiceLabel  string
	DecisionLabel string
	Automatic     bool
	Selectable    bool
	Carried       bool
}

type revisionSourceVisa struct {
	VisaID    int64
	FileID    int64
	VersionNo int
	Service   string
	Decision  string
	Comment   string
	DecidedBy int64
	DecidedAt time.Time
}

type revisionFile struct {
	ID             int64
	Title          string
	CurrentVersion int
}

func revisionVisaKey(fileID int64, service string) string {
	return strconv.FormatInt(fileID, 10) + ":" + service
}

func buildRevisionPlan(files []revisionFile, sources map[string]revisionSourceVisa, selected map[string]bool) (revisionPlanView, int, int, error) {
	plan := revisionPlanView{Available: true, Ready: true}
	freshCount := 0
	carriedCount := 0
	allowedSelections := make(map[string]bool)
	for _, file := range files {
		view := revisionFileView{ID: file.ID, Title: file.Title, CurrentVersion: file.CurrentVersion}
		for _, service := range requiredInternalServices {
			key := revisionVisaKey(file.ID, service)
			source, found := sources[key]
			serviceView := revisionServiceView{Value: key, ServiceLabel: internalServiceLabel(service)}
			if !found {
				view.IsNew = true
				view.Changed = true
				serviceView.Automatic = true
				serviceView.DecisionLabel = "Новая позиция — требуется виза"
				freshCount++
				view.Services = append(view.Services, serviceView)
				continue
			}
			if view.PreviousVersion == 0 {
				view.PreviousVersion = source.VersionNo
			}
			view.Changed = file.CurrentVersion != source.VersionNo
			serviceView.DecisionLabel = internalDecisionLabel(source.Decision)
			if source.Decision == "rejected" {
				serviceView.Automatic = true
				if !view.Changed {
					plan.Ready = false
					plan.BlockReason = "Для каждого несогласованного файла загрузите и подтвердите новую версию"
				} else {
					freshCount++
				}
			} else if view.Changed {
				serviceView.Selectable = true
				allowedSelections[key] = true
				if selected[key] {
					freshCount++
				} else {
					serviceView.Carried = true
					carriedCount++
				}
			} else {
				serviceView.Carried = true
				carriedCount++
			}
			view.Services = append(view.Services, serviceView)
		}
		plan.Files = append(plan.Files, view)
	}
	for value := range selected {
		if !allowedSelections[value] {
			return revisionPlanView{}, 0, 0, errors.New("Выбрано недопустимое дополнительное согласование")
		}
	}
	return plan, freshCount, carriedCount, nil
}

func (app *application) loadRevisionPlan(ctx context.Context, questionID int64, questionStatus string, usr user) (revisionPlanView, error) {
	return loadRevisionPlan(ctx, app.db, questionID, questionStatus, usr)
}

type revisionPlanDB interface {
	revisionQueryer
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadRevisionPlan(ctx context.Context, db revisionPlanDB, questionID int64, questionStatus string, usr user) (revisionPlanView, error) {
	if questionStatus != "revision_required" {
		return revisionPlanView{}, nil
	}
	var previousRoundID int64
	err := db.QueryRow(ctx, `
		SELECT id FROM internal_review_rounds
		WHERE question_id = $1 AND status = 'completed'
		ORDER BY completed_at DESC, id DESC LIMIT 1
	`, questionID).Scan(&previousRoundID)
	if err != nil {
		return revisionPlanView{}, err
	}
	files, err := loadRevisionFiles(ctx, db, questionID)
	if err != nil {
		return revisionPlanView{}, err
	}
	sources, err := loadRevisionSourceVisas(ctx, db, previousRoundID)
	if err != nil {
		return revisionPlanView{}, err
	}
	plan, _, _, err := buildRevisionPlan(files, sources, nil)
	if err != nil {
		return revisionPlanView{}, err
	}
	plan.CanRestart = canManageQuestions(usr)
	plan.DeadlineValue = time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	var pending int
	if err := db.QueryRow(ctx, `
		SELECT COUNT(*) FROM question_file_versions v
		JOIN question_files f ON f.id = v.question_file_id
		WHERE f.question_id = $1 AND f.status = 'active' AND v.approval_status = 'pending'
	`, questionID).Scan(&pending); err != nil {
		return revisionPlanView{}, err
	}
	if pending > 0 {
		plan.Ready = false
		plan.BlockReason = "Сначала подтвердите или отклоните ожидающие загрузки"
	}
	return plan, nil
}

type revisionQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func loadRevisionFiles(ctx context.Context, queryer revisionQueryer, questionID int64) ([]revisionFile, error) {
	rows, err := queryer.Query(ctx, `
		SELECT id, title, current_version_no FROM question_files
		WHERE question_id = $1 AND status = 'active' AND current_version_no IS NOT NULL
		ORDER BY created_at, id
	`, questionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []revisionFile
	for rows.Next() {
		var file revisionFile
		if err := rows.Scan(&file.ID, &file.Title, &file.CurrentVersion); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

func loadRevisionSourceVisas(ctx context.Context, queryer revisionQueryer, roundID int64) (map[string]revisionSourceVisa, error) {
	rows, err := queryer.Query(ctx, `
		SELECT COALESCE(v.source_visa_id, v.id), r.question_file_id, r.version_no, r.internal_service,
		       v.decision, v.comment, v.decided_by, v.decided_at
		FROM internal_review_requirements r
		JOIN internal_review_visas v ON v.requirement_id = r.id AND v.withdrawn_at IS NULL
		WHERE r.round_id = $1 AND r.status = 'responded'
	`, roundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]revisionSourceVisa)
	for rows.Next() {
		var source revisionSourceVisa
		if err := rows.Scan(&source.VisaID, &source.FileID, &source.VersionNo, &source.Service,
			&source.Decision, &source.Comment, &source.DecidedBy, &source.DecidedAt); err != nil {
			return nil, err
		}
		result[revisionVisaKey(source.FileID, source.Service)] = source
	}
	return result, rows.Err()
}

func (app *application) restartInternalReview(c *gin.Context) {
	usr := c.MustGet("user").(user)
	questionID, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	deadline, err := time.Parse("2006-01-02", strings.TrimSpace(c.PostForm("deadline")))
	if err != nil {
		respondMessage(c, http.StatusUnprocessableEntity, "Укажите новый срок внутреннего согласования")
		return
	}
	today := time.Now()
	todayDate := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	if deadline.Before(todayDate) {
		respondMessage(c, http.StatusUnprocessableEntity, "Срок внутреннего согласования не может быть в прошлом")
		return
	}
	selected := make(map[string]bool)
	for _, value := range c.PostFormArray("review_service") {
		selected[strings.TrimSpace(value)] = true
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		respondMessage(c, http.StatusInternalServerError, "Не удалось начать повторное согласование")
		return
	}
	defer tx.Rollback(c.Request.Context())
	app.restartInternalReviewInTx(c, tx, usr, questionID, deadline, selected)
}

func (app *application) restartInternalReviewInTx(c *gin.Context, tx pgx.Tx, usr user, questionID int64, deadline time.Time, selected map[string]bool) {
	if err := lockUserAssignments(c, tx); err != nil {
		respondMessage(c, http.StatusInternalServerError, "Не удалось проверить назначения пользователей")
		return
	}
	var title, status string
	var updatedAt time.Time
	err := tx.QueryRow(c.Request.Context(), `SELECT title, status, updated_at FROM questions WHERE id = $1 FOR UPDATE`, questionID).Scan(&title, &status, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		respondMessage(c, http.StatusNotFound, "Вопрос не найден")
		return
	}
	if err != nil {
		respondMessage(c, http.StatusInternalServerError, "Не удалось загрузить вопрос")
		return
	}
	if status != "revision_required" {
		respondMessage(c, http.StatusConflict, "Повторное согласование сейчас недоступно")
		return
	}
	if !committeeContextMatches(c.PostForm("question_context"), updatedAt) {
		respondMessage(c, http.StatusConflict, "Вопрос изменился. Обновите карточку и проверьте комплект и перенос виз перед повторным согласованием")
		return
	}
	var previousRoundID int64
	err = tx.QueryRow(c.Request.Context(), `
		SELECT id FROM internal_review_rounds
		WHERE question_id = $1 AND status = 'completed'
		ORDER BY completed_at DESC, id DESC LIMIT 1 FOR UPDATE
	`, questionID).Scan(&previousRoundID)
	if err != nil {
		respondMessage(c, http.StatusInternalServerError, "Не удалось найти предыдущий раунд")
		return
	}
	files, err := loadRevisionFiles(c.Request.Context(), tx, questionID)
	if err != nil || len(files) == 0 {
		respondMessage(c, http.StatusUnprocessableEntity, "Вопрос не содержит официальных файлов")
		return
	}
	sources, err := loadRevisionSourceVisas(c.Request.Context(), tx, previousRoundID)
	if err != nil {
		respondMessage(c, http.StatusInternalServerError, "Не удалось загрузить прежние визы")
		return
	}
	plan, freshCount, carriedCount, err := buildRevisionPlan(files, sources, selected)
	if err != nil {
		respondMessage(c, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if !plan.Ready {
		respondMessage(c, http.StatusUnprocessableEntity, plan.BlockReason)
		return
	}
	var pendingUploads int
	if err := tx.QueryRow(c.Request.Context(), `
		SELECT COUNT(*) FROM question_file_versions v JOIN question_files f ON f.id = v.question_file_id
		WHERE f.question_id = $1 AND f.status = 'active' AND v.approval_status = 'pending'
	`, questionID).Scan(&pendingUploads); err != nil {
		respondMessage(c, http.StatusInternalServerError, "Не удалось проверить ожидающие загрузки")
		return
	}
	if pendingUploads > 0 {
		respondMessage(c, http.StatusConflict, "Сначала подтвердите или отклоните ожидающие загрузки")
		return
	}
	activeServices, err := loadActiveServiceCounts(c.Request.Context(), tx)
	if err != nil {
		respondMessage(c, http.StatusInternalServerError, "Не удалось проверить представителей дирекций")
		return
	}
	for _, service := range requiredInternalServices {
		if activeServices[service] < 1 {
			respondMessage(c, http.StatusConflict, "Нет активного представителя: "+internalServiceLabel(service))
			return
		}
	}
	var roundID int64
	err = tx.QueryRow(c.Request.Context(), `
		INSERT INTO internal_review_rounds (question_id, deadline, started_by, frozen_decision_text)
		SELECT id, $2, $3, decision_text FROM questions WHERE id = $1 RETURNING id
	`, questionID, deadline, usr.ID).Scan(&roundID)
	if err != nil {
		respondMessage(c, http.StatusInternalServerError, "Не удалось создать повторный раунд")
		return
	}
	for _, file := range files {
		for _, service := range requiredInternalServices {
			key := revisionVisaKey(file.ID, service)
			source, found := sources[key]
			fresh := !found || source.Decision == "rejected" || (file.CurrentVersion != source.VersionNo && selected[key])
			requirementStatus := "responded"
			if fresh {
				requirementStatus = "pending"
			}
			var requirementID int64
			err = tx.QueryRow(c.Request.Context(), `
				INSERT INTO internal_review_requirements (
					round_id, question_file_id, version_no, internal_service, status
				) VALUES ($1, $2, $3, $4, $5) RETURNING id
			`, roundID, file.ID, file.CurrentVersion, service, requirementStatus).Scan(&requirementID)
			if err != nil {
				respondMessage(c, http.StatusInternalServerError, "Не удалось сформировать повторные задания")
				return
			}
			if !fresh {
				_, err = tx.Exec(c.Request.Context(), `
					INSERT INTO internal_review_visas (
						requirement_id, decision, comment, decided_by, decided_at,
						source_visa_id, carried_by, carried_at
					) VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
				`, requirementID, source.Decision, source.Comment, source.DecidedBy,
					source.DecidedAt, source.VisaID, usr.ID)
				if err != nil {
					respondMessage(c, http.StatusInternalServerError, "Не удалось перенести прежние визы")
					return
				}
			}
		}
	}
	_, err = tx.Exec(c.Request.Context(), `
		UPDATE questions SET status = 'internal_review', internal_deadline = $2, updated_at = NOW()
		WHERE id = $1
	`, questionID, deadline)
	if err == nil {
		err = app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
			EventType: "internal_review.restarted", TargetType: "question", TargetID: &questionID,
			TargetLabel: title, QuestionID: &questionID,
			Details: fmt.Sprintf("Раунд №%d после доработки: новых виз %d, перенесено %d", roundID, freshCount, carriedCount),
		})
	}
	if err == nil {
		err = app.finishInternalReviewIfComplete(c.Request.Context(), tx, usr, questionID, roundID, title)
	}
	if err == nil {
		err = tx.Commit(c.Request.Context())
	}
	if err != nil {
		respondMessage(c, http.StatusInternalServerError, "Не удалось запустить повторное согласование")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", questionID))
}

func loadActiveServiceCounts(ctx context.Context, queryer revisionQueryer) (map[string]int, error) {
	rows, err := queryer.Query(ctx, `
		SELECT internal_service, COUNT(*) FROM users
		WHERE active = TRUE AND role = 'approver' AND internal_service IS NOT NULL
		GROUP BY internal_service
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]int)
	for rows.Next() {
		var service string
		var count int
		if err := rows.Scan(&service, &count); err != nil {
			return nil, err
		}
		result[service] = count
	}
	return result, rows.Err()
}
