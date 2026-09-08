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
	"github.com/jackc/pgx/v5/pgconn"
)

type auditExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type auditRecord struct {
	EventType   string
	TargetType  string
	TargetID    *int64
	TargetLabel string
	DocumentID  *int64
	QuestionID  *int64
	VersionNo   *int
	Details     string
}

type auditListItem struct {
	ID          int64
	ActorName   string
	ActorEmail  string
	ActorRole   string
	EventLabel  string
	TargetLabel string
	ObjectMeta  string
	Details     string
	CreatedAt   string
}

type auditActor struct {
	ID       int64
	FullName string
}

type auditFilters struct {
	EventType  string
	ActorID    int64
	DocumentID int64
	DateFrom   string
	DateTo     string
}

func (app *application) writeAudit(ctx context.Context, executor auditExecutor, actor user, record auditRecord) error {
	if strings.TrimSpace(record.EventType) == "" {
		return errors.New("audit event type is required")
	}
	_, err := executor.Exec(ctx, `
		INSERT INTO audit_events (
			actor_user_id, actor_name, actor_email, actor_role, event_type,
			target_type, target_id, target_label, document_id, question_id, version_no, details
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, actor.ID, actor.FullName, actor.Email, actor.Role, record.EventType,
		record.TargetType, record.TargetID, record.TargetLabel, record.DocumentID,
		record.QuestionID, record.VersionNo, record.Details)
	return err
}

func (app *application) showAuditLog(c *gin.Context) {
	filters, err := parseAuditFilters(c)
	if err != nil {
		respondMessage(c, http.StatusBadRequest, err.Error())
		return
	}

	rows, err := app.db.Query(c.Request.Context(), `
		SELECT id, actor_name, actor_email, actor_role, event_type,
		       target_label, COALESCE(document_id, 0), COALESCE(question_id, 0),
		       COALESCE(version_no, 0), details, created_at
		FROM audit_events
		WHERE ($1 = '' OR event_type = $1)
		  AND ($2::BIGINT = 0 OR actor_user_id = $2)
		  AND ($3::BIGINT = 0 OR document_id = $3)
		  AND ($4 = '' OR created_at >= $4::DATE)
		  AND ($5 = '' OR created_at < ($5::DATE + INTERVAL '1 day'))
		ORDER BY created_at DESC, id DESC
		LIMIT 200
	`, filters.EventType, filters.ActorID, filters.DocumentID, filters.DateFrom, filters.DateTo)
	if err != nil {
		respondMessage(c, http.StatusInternalServerError, "Не удалось загрузить журнал аудита")
		return
	}
	defer rows.Close()

	var events []auditListItem
	for rows.Next() {
		var item auditListItem
		var eventType, role string
		var documentID int64
		var questionID int64
		var versionNo int
		var createdAt time.Time
		if err := rows.Scan(&item.ID, &item.ActorName, &item.ActorEmail, &role, &eventType,
			&item.TargetLabel, &documentID, &questionID, &versionNo, &item.Details, &createdAt); err != nil {
			respondMessage(c, http.StatusInternalServerError, "Не удалось прочитать журнал аудита")
			return
		}
		item.ActorRole = roleLabel(role)
		item.EventLabel = auditEventLabel(eventType)
		if documentID > 0 {
			item.ObjectMeta = fmt.Sprintf("Документ №%d", documentID)
			if versionNo > 0 {
				item.ObjectMeta += fmt.Sprintf(" · версия %d", versionNo)
			}
		}
		if questionID > 0 {
			item.ObjectMeta = fmt.Sprintf("Вопрос №%d", questionID)
		}
		item.CreatedAt = createdAt.Format("02.01.2006 15:04:05")
		events = append(events, item)
	}
	if err := rows.Err(); err != nil {
		respondMessage(c, http.StatusInternalServerError, "Не удалось прочитать журнал аудита")
		return
	}

	actors, err := app.listAuditActors(c.Request.Context())
	if err != nil {
		respondMessage(c, http.StatusInternalServerError, "Не удалось загрузить список пользователей")
		return
	}
	c.HTML(http.StatusOK, "audit.html", gin.H{
		"Title":     "Журнал аудита",
		"User":      c.MustGet("user").(user),
		"CSRFToken": app.templateCSRF(c),
		"Events":    events,
		"Actors":    actors,
		"Filters":   filters,
	})
}

func (app *application) listAuditActors(ctx context.Context) ([]auditActor, error) {
	rows, err := app.db.Query(ctx, `
		SELECT id, full_name
		FROM (
			SELECT DISTINCT ON (actor_user_id)
			       actor_user_id AS id, actor_name AS full_name
			FROM audit_events
			ORDER BY actor_user_id, created_at DESC, audit_events.id DESC
		) actors
		ORDER BY full_name, id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var actors []auditActor
	for rows.Next() {
		var actor auditActor
		if err := rows.Scan(&actor.ID, &actor.FullName); err != nil {
			return nil, err
		}
		actors = append(actors, actor)
	}
	return actors, rows.Err()
}

func parseAuditFilters(c *gin.Context) (auditFilters, error) {
	filters := auditFilters{
		EventType: strings.TrimSpace(c.Query("event_type")),
		DateFrom:  strings.TrimSpace(c.Query("date_from")),
		DateTo:    strings.TrimSpace(c.Query("date_to")),
	}
	var err error
	if value := strings.TrimSpace(c.Query("actor_id")); value != "" {
		filters.ActorID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || filters.ActorID < 1 {
			return auditFilters{}, errors.New("Некорректный пользователь в фильтре")
		}
	}
	if value := strings.TrimSpace(c.Query("document_id")); value != "" {
		filters.DocumentID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || filters.DocumentID < 1 {
			return auditFilters{}, errors.New("Некорректный номер документа в фильтре")
		}
	}
	for _, value := range []string{filters.DateFrom, filters.DateTo} {
		if value != "" {
			if _, err := time.Parse("2006-01-02", value); err != nil {
				return auditFilters{}, errors.New("Некорректная дата в фильтре")
			}
		}
	}
	return filters, nil
}

func auditEventLabel(eventType string) string {
	labels := map[string]string{
		"admin.reset":                     "Полный сброс данных",
		"admin.archive_all":               "Материалы архивированы, доступ пользователей отключён",
		"admin.archive_question":          "Вопрос помещён в архив",
		"admin.archive_document":          "Документ помещён в архив",
		"user.restored":                   "Восстановлен доступ пользователя",
		"user.sessions_revoked":           "Завершены сессии пользователя",
		"session.login":                   "Вход в систему",
		"session.logout":                  "Выход из системы",
		"user.created":                    "Создан пользователь",
		"user.password_reset":             "Сброшен пароль пользователя",
		"user.deactivated":                "Отключён пользователь",
		"user.password_changed":           "Изменён собственный пароль",
		"user.assignment_updated":         "Обновлено назначение пользователя",
		"question.created":                "Создан вопрос",
		"question.updated":                "Изменён черновик вопроса",
		"question.decision_revised":       "Создана редакция решения",
		"question.cancelled":              "Отменён вопрос",
		"question.file_version_uploaded":  "Загружена версия файла вопроса",
		"question.file_excluded":          "Файл исключён из комплекта",
		"question.file_version_confirmed": "Подтверждена версия файла вопроса",
		"question.file_version_rejected":  "Отклонена версия файла вопроса",
		"internal_review.started":         "Запущено внутреннее согласование",
		"internal_review.visa_submitted":  "Поставлена внутренняя виза",
		"internal_review.visa_withdrawn":  "Отозвана внутренняя виза",
		"internal_review.extended":        "Продлён срок внутреннего согласования",
		"internal_review.completed":       "Завершено внутреннее согласование",
		"internal_review.cancelled":       "Отменено внутреннее согласование",
		"internal_review.restarted":       "Запущено повторное внутреннее согласование",
		"committee_vote.started":          "Запущено голосование Комитета",
		"committee_vote.submitted":        "Подан голос Комитета",
		"committee_vote.withdrawn":        "Отозван голос Комитета",
		"committee_vote.extended":         "Продлён срок голосования Комитета",
		"committee_vote.completed":        "Завершено голосование Комитета",
		"committee_vote.cancelled":        "Отменено голосование Комитета",
		"protocol.created":                "Сформирован протокол",
		"protocol.deleted":                "Удалён протокол",
		"document.created":                "Создан документ",
		"document.version_uploaded":       "Загружена версия документа",
		"approval.started":                "Запущено согласование",
		"approval.response_submitted":     "Отправлено решение",
		"approval.completed":              "Завершено согласование",
		"approval.cancelled":              "Отменено согласование",
	}
	if label, ok := labels[eventType]; ok {
		return label
	}
	return fmt.Sprintf("Событие: %s", eventType)
}
