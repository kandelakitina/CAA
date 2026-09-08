package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

const protocolPlace = "Заочное голосование с использованием информационной системы"

var moscowLocation = time.FixedZone("MSK", 3*60*60)

type protocolListItem struct {
	ID            int64
	Number        string
	MeetingLabel  string
	CreatedLabel  string
	QuestionCount int
}

type protocolCandidate struct {
	ID           int64
	Title        string
	DecisionText string
	CompletedAt  string
}

type protocolSelection struct {
	QuestionID int64
	Order      int
}

type protocolView struct {
	ID            int64
	Number        string
	DateLabel     string
	TimeLabel     string
	Place         string
	ChairName     string
	SecretaryName string
	CreatedLabel  string
	CanDelete     bool
	Questions     []protocolQuestionView
}

type protocolQuestionView struct {
	Order        int
	QuestionID   int64
	Title        string
	Summary      string
	DecisionText string
	Outcome      string
	Files        []protocolFileView
	Participants []protocolParticipantView
}

type protocolFileView struct {
	Title     string
	VersionNo int
}

type protocolParticipantView struct {
	Name            string
	IsChair         bool
	Responded       bool
	Decision        string
	Comment         string
	AttachmentCount int
}

func protocolNumber(sequence, year int) string {
	return fmt.Sprintf("%d/%d", sequence, year)
}

func parseProtocolSelections(form map[string][]string) ([]protocolSelection, error) {
	values := form["question_id"]
	if len(values) == 0 {
		return nil, errors.New("Выберите хотя бы один вопрос")
	}
	if len(values) > 100 {
		return nil, errors.New("В один протокол можно включить не более 100 вопросов")
	}
	seenQuestions := make(map[int64]bool, len(values))
	seenOrders := make(map[int]bool, len(values))
	result := make([]protocolSelection, 0, len(values))
	for _, value := range values {
		questionID, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || questionID < 1 || seenQuestions[questionID] {
			return nil, errors.New("Список вопросов содержит некорректные данные")
		}
		orderValue := ""
		if orders := form[fmt.Sprintf("order_%d", questionID)]; len(orders) > 0 {
			orderValue = strings.TrimSpace(orders[0])
		}
		order, err := strconv.Atoi(orderValue)
		if err != nil || order < 1 || order > len(values) || seenOrders[order] {
			return nil, errors.New("Задайте уникальный порядок вопросов от 1 до количества выбранных")
		}
		seenQuestions[questionID] = true
		seenOrders[order] = true
		result = append(result, protocolSelection{QuestionID: questionID, Order: order})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Order < result[j].Order })
	for index, item := range result {
		if item.Order != index+1 {
			return nil, errors.New("Порядок вопросов должен быть последовательным, начиная с 1")
		}
	}
	return result, nil
}

func (app *application) listProtocols(c *gin.Context) {
	usr := c.MustGet("user").(user)
	protocols, err := app.loadProtocolList(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить протоколы")
		return
	}
	candidates, err := app.loadProtocolCandidates(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить вопросы для протокола")
		return
	}
	c.HTML(http.StatusOK, "protocols.html", gin.H{
		"Title": "Протоколы", "User": usr, "CSRFToken": app.templateCSRF(c),
		"Protocols": protocols, "Candidates": candidates,
	})
}

func (app *application) loadProtocolList(ctx context.Context) ([]protocolListItem, error) {
	rows, err := app.db.Query(ctx, `
		SELECT protocol.id, protocol.sequence_no, protocol.year, protocol.meeting_at,
		       protocol.created_at, COUNT(item.question_id)
		FROM protocols protocol
		JOIN protocol_questions item ON item.protocol_id = protocol.id
		GROUP BY protocol.id
		ORDER BY protocol.year DESC, protocol.sequence_no DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []protocolListItem
	for rows.Next() {
		var item protocolListItem
		var sequence, year int
		var meetingAt, createdAt time.Time
		if err := rows.Scan(&item.ID, &sequence, &year, &meetingAt, &createdAt, &item.QuestionCount); err != nil {
			return nil, err
		}
		item.Number = protocolNumber(sequence, year)
		item.MeetingLabel = meetingAt.In(moscowLocation).Format("02.01.2006 15:04")
		item.CreatedLabel = createdAt.In(moscowLocation).Format("02.01.2006 15:04")
		result = append(result, item)
	}
	return result, rows.Err()
}

func (app *application) loadProtocolCandidates(ctx context.Context) ([]protocolCandidate, error) {
	rows, err := app.db.Query(ctx, `
		SELECT question.id, question.title, round.frozen_decision_text, round.completed_at
		FROM questions question
		JOIN committee_vote_rounds round ON round.question_id = question.id
		WHERE question.status = 'approved' AND round.status = 'completed' AND round.outcome = 'approved'
		  AND NOT EXISTS (SELECT 1 FROM protocol_questions item WHERE item.question_id = question.id)
		ORDER BY round.completed_at, question.id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []protocolCandidate
	for rows.Next() {
		var item protocolCandidate
		var completedAt time.Time
		if err := rows.Scan(&item.ID, &item.Title, &item.DecisionText, &completedAt); err != nil {
			return nil, err
		}
		item.CompletedAt = completedAt.In(moscowLocation).Format("02.01.2006 15:04")
		result = append(result, item)
	}
	return result, rows.Err()
}

func (app *application) createProtocol(c *gin.Context) {
	usr := c.MustGet("user").(user)
	if err := c.Request.ParseForm(); err != nil {
		c.String(http.StatusBadRequest, "Не удалось прочитать состав протокола")
		return
	}
	selections, err := parseProtocolSelections(c.Request.PostForm)
	if err != nil {
		c.String(http.StatusUnprocessableEntity, err.Error())
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать формирование протокола")
		return
	}
	defer tx.Rollback(c.Request.Context())
	type selectedQuestion struct {
		protocolSelection
		RoundID      int64
		Title        string
		Summary      string
		DecisionText string
		ChairName    string
		LastVote     time.Time
	}
	selected := make([]selectedQuestion, 0, len(selections))
	var meetingAt time.Time
	chairName := ""
	for _, selection := range selections {
		var item selectedQuestion
		item.protocolSelection = selection
		err := tx.QueryRow(c.Request.Context(), `
			SELECT round.id, question.title, question.summary, round.frozen_decision_text,
			       chair.name_snapshot,
			       (SELECT MAX(vote.voted_at)
			        FROM committee_vote_participants participant
			        JOIN committee_votes vote ON vote.participant_id = participant.id
			        WHERE participant.round_id = round.id AND vote.withdrawn_at IS NULL)
			FROM questions question
			JOIN committee_vote_rounds round ON round.question_id = question.id
			JOIN committee_vote_participants chair ON chair.round_id = round.id AND chair.is_chair_snapshot = TRUE
			WHERE question.id = $1 AND question.status = 'approved'
			  AND round.status = 'completed' AND round.outcome = 'approved'
			  AND NOT EXISTS (SELECT 1 FROM protocol_questions existing WHERE existing.question_id = question.id)
			ORDER BY round.completed_at DESC, round.id DESC
			LIMIT 1 FOR UPDATE OF question, round
		`, selection.QuestionID).Scan(&item.RoundID, &item.Title, &item.Summary, &item.DecisionText, &item.ChairName, &item.LastVote)
		if errors.Is(err, pgx.ErrNoRows) {
			c.String(http.StatusConflict, "Один из вопросов уже включён в протокол или больше не доступен")
			return
		}
		if err != nil {
			c.String(http.StatusInternalServerError, "Не удалось проверить вопросы протокола")
			return
		}
		if chairName == "" {
			chairName = item.ChairName
		} else if chairName != item.ChairName {
			c.String(http.StatusConflict, "Вопросы с разными председателями нужно оформить отдельными протоколами")
			return
		}
		if item.LastVote.After(meetingAt) {
			meetingAt = item.LastVote
		}
		selected = append(selected, item)
	}
	year := meetingAt.In(moscowLocation).Year()
	var sequence int
	err = tx.QueryRow(c.Request.Context(), `
		INSERT INTO protocol_sequences (year, last_number) VALUES ($1, 1)
		ON CONFLICT (year) DO UPDATE SET last_number = protocol_sequences.last_number + 1
		RETURNING last_number
	`, year).Scan(&sequence)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось присвоить номер протокола")
		return
	}
	var protocolID int64
	err = tx.QueryRow(c.Request.Context(), `
		INSERT INTO protocols (
			year, sequence_no, meeting_at, chair_name,
			secretary_user_id, secretary_name, secretary_email
		) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id
	`, year, sequence, meetingAt, chairName, usr.ID, usr.FullName, usr.Email).Scan(&protocolID)
	for _, item := range selected {
		if err != nil {
			break
		}
		_, err = tx.Exec(c.Request.Context(), `
			INSERT INTO protocol_questions (
				protocol_id, question_id, round_id, agenda_order,
				title_snapshot, summary_snapshot, decision_text_snapshot
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, protocolID, item.QuestionID, item.RoundID, item.Order, item.Title, item.Summary, item.DecisionText)
	}
	if err == nil {
		number := protocolNumber(sequence, year)
		err = app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
			EventType: "protocol.created", TargetType: "protocol", TargetID: &protocolID,
			TargetLabel: "Протокол №" + number,
			Details:     fmt.Sprintf("Вопросов: %d. Дата голосования: %s", len(selected), meetingAt.In(moscowLocation).Format("02.01.2006 15:04")),
		})
	}
	if err == nil {
		err = tx.Commit(c.Request.Context())
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось сохранить протокол")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/protocols/%d", protocolID))
}

func (app *application) showProtocol(c *gin.Context) {
	protocolID, ok := parsePositiveID(c, "id", "Некорректный идентификатор протокола")
	if !ok {
		return
	}
	view, err := app.loadProtocol(c.Request.Context(), protocolID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Протокол не найден")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить протокол")
		return
	}
	usr := c.MustGet("user").(user)
	view.CanDelete = usr.Role == "secretary"
	c.HTML(http.StatusOK, "protocol.html", gin.H{
		"Title": "Протокол №" + view.Number, "User": usr,
		"CSRFToken": app.templateCSRF(c), "Protocol": view,
	})
}

func (app *application) loadProtocol(ctx context.Context, protocolID int64) (protocolView, error) {
	var view protocolView
	var sequence, year int
	var meetingAt, createdAt time.Time
	err := app.db.QueryRow(ctx, `
		SELECT id, sequence_no, year, meeting_at, chair_name, secretary_name, created_at
		FROM protocols WHERE id = $1
	`, protocolID).Scan(&view.ID, &sequence, &year, &meetingAt, &view.ChairName, &view.SecretaryName, &createdAt)
	if err != nil {
		return protocolView{}, err
	}
	view.Number = protocolNumber(sequence, year)
	view.DateLabel = meetingAt.In(moscowLocation).Format("02.01.2006")
	view.TimeLabel = meetingAt.In(moscowLocation).Format("15:04")
	view.Place = protocolPlace
	view.CreatedLabel = createdAt.In(moscowLocation).Format("02.01.2006 15:04")
	rows, err := app.db.Query(ctx, `
		SELECT agenda_order, question_id, title_snapshot, summary_snapshot,
		       decision_text_snapshot, round_id
		FROM protocol_questions WHERE protocol_id = $1 ORDER BY agenda_order
	`, protocolID)
	if err != nil {
		return protocolView{}, err
	}
	type questionRound struct {
		Question protocolQuestionView
		RoundID  int64
	}
	var questionRounds []questionRound
	for rows.Next() {
		var item protocolQuestionView
		var roundID int64
		if err := rows.Scan(&item.Order, &item.QuestionID, &item.Title, &item.Summary, &item.DecisionText, &roundID); err != nil {
			rows.Close()
			return protocolView{}, err
		}
		item.Outcome = "Решение принято"
		questionRounds = append(questionRounds, questionRound{Question: item, RoundID: roundID})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return protocolView{}, err
	}
	rows.Close()
	for _, stored := range questionRounds {
		item := stored.Question
		files, err := app.loadProtocolFiles(ctx, stored.RoundID)
		if err != nil {
			return protocolView{}, err
		}
		item.Files = files
		participants, err := app.loadProtocolParticipants(ctx, stored.RoundID)
		if err != nil {
			return protocolView{}, err
		}
		item.Participants = participants
		view.Questions = append(view.Questions, item)
	}
	return view, nil
}

func (app *application) loadProtocolFiles(ctx context.Context, roundID int64) ([]protocolFileView, error) {
	rows, err := app.db.Query(ctx, `
		SELECT file.title, frozen.version_no
		FROM committee_vote_round_files frozen
		JOIN question_files file ON file.id = frozen.question_file_id
		WHERE frozen.round_id = $1 ORDER BY file.created_at, file.id
	`, roundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []protocolFileView
	for rows.Next() {
		var item protocolFileView
		if err := rows.Scan(&item.Title, &item.VersionNo); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (app *application) loadProtocolParticipants(ctx context.Context, roundID int64) ([]protocolParticipantView, error) {
	rows, err := app.db.Query(ctx, `
		SELECT participant.name_snapshot, participant.is_chair_snapshot,
		       vote.id IS NOT NULL, COALESCE(vote.decision, ''), COALESCE(vote.comment, ''),
		       COUNT(attachment.id)
		FROM committee_vote_participants participant
		LEFT JOIN committee_votes vote ON vote.participant_id = participant.id AND vote.withdrawn_at IS NULL
		LEFT JOIN committee_vote_attachments attachment ON attachment.vote_id = vote.id
		WHERE participant.round_id = $1
		GROUP BY participant.id, vote.id
		ORDER BY participant.name_snapshot, participant.id
	`, roundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []protocolParticipantView
	for rows.Next() {
		var item protocolParticipantView
		if err := rows.Scan(&item.Name, &item.IsChair, &item.Responded, &item.Decision, &item.Comment, &item.AttachmentCount); err != nil {
			return nil, err
		}
		item.Decision = committeeDecisionLabel(item.Decision)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (app *application) downloadProtocolWord(c *gin.Context) {
	protocolID, ok := parsePositiveID(c, "id", "Некорректный идентификатор протокола")
	if !ok {
		return
	}
	view, err := app.loadProtocol(c.Request.Context(), protocolID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Протокол не найден")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось сформировать Word-файл")
		return
	}
	content, err := renderProtocolWord(view)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось сформировать Word-файл")
		return
	}
	filename := fmt.Sprintf("protocol-%s.doc", strings.ReplaceAll(view.Number, "/", "-"))
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	c.Header("Cache-Control", "private, no-store")
	c.Data(http.StatusOK, "application/msword; charset=utf-8", content)
}

func renderProtocolWord(view protocolView) ([]byte, error) {
	tmpl, err := template.New("protocol-word").Parse(protocolWordTemplate)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	output.Write([]byte{0xEF, 0xBB, 0xBF})
	if err := tmpl.Execute(&output, view); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (app *application) deleteProtocol(c *gin.Context) {
	usr := c.MustGet("user").(user)
	protocolID, ok := parsePositiveID(c, "id", "Некорректный идентификатор протокола")
	if !ok {
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать удаление протокола")
		return
	}
	defer tx.Rollback(c.Request.Context())
	var sequence, year int
	err = tx.QueryRow(c.Request.Context(), `
		SELECT sequence_no, year FROM protocols WHERE id = $1 FOR UPDATE
	`, protocolID).Scan(&sequence, &year)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Протокол не найден")
		return
	}
	if err == nil {
		_, err = tx.Exec(c.Request.Context(), `DELETE FROM protocols WHERE id = $1`, protocolID)
	}
	if err == nil {
		number := protocolNumber(sequence, year)
		err = app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
			EventType: "protocol.deleted", TargetType: "protocol", TargetID: &protocolID,
			TargetLabel: "Протокол №" + number,
			Details:     "Вопросы снова доступны для формирования протокола",
		})
	}
	if err == nil {
		err = tx.Commit(c.Request.Context())
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось удалить протокол")
		return
	}
	c.Redirect(http.StatusSeeOther, "/protocols")
}

const protocolWordTemplate = `<!doctype html>
<html><head><meta charset="utf-8"><style>
body { font-family: "Times New Roman", serif; font-size: 12pt; line-height: 1.35; }
h1, .center { text-align: center; } h1 { font-size: 16pt; }
table { width: 100%; border-collapse: collapse; margin: 12pt 0; }
th, td { border: 1px solid #000; padding: 5pt; vertical-align: top; }
.signature { margin-top: 36pt; display: table; width: 100%; }
.signature p { display: table-row; } .signature span { display: table-cell; padding: 14pt 0; }
</style></head><body>
<h1>ПРОТОКОЛ № {{.Number}}</h1>
<p class="center">заочного голосования Комитета</p>
<p><strong>Дата:</strong> {{.DateLabel}} &nbsp; <strong>Время:</strong> {{.TimeLabel}}</p>
<p><strong>Форма проведения:</strong> заочное голосование</p>
<p><strong>Место:</strong> {{.Place}}</p>
<p><strong>Председатель:</strong> {{.ChairName}}<br><strong>Секретарь:</strong> {{.SecretaryName}}</p>
<h2>Повестка дня</h2><ol>{{range .Questions}}<li>{{.Title}}</li>{{end}}</ol>
{{range .Questions}}
<h2>{{.Order}}. {{.Title}}</h2>
{{if .Summary}}<p><strong>Краткое описание:</strong> {{.Summary}}</p>{{end}}
<p><strong>Материалы:</strong></p><ul>{{range .Files}}<li>{{.Title}}, версия {{.VersionNo}}</li>{{end}}</ul>
<p><strong>Формулировка решения:</strong> {{.DecisionText}}</p>
<table><thead><tr><th>Участник</th><th>Участие</th><th>Голос</th><th>Комментарий</th></tr></thead><tbody>
{{range .Participants}}<tr><td>{{.Name}}{{if .IsChair}} (председатель){{end}}</td><td>{{if .Responded}}проголосовал{{else}}отсутствовал{{end}}</td><td>{{if .Responded}}{{.Decision}}{{else}}—{{end}}</td><td>{{if .Comment}}{{.Comment}}{{else}}—{{end}}{{if .AttachmentCount}}<br>Приложено файлов: {{.AttachmentCount}}{{end}}</td></tr>{{end}}
</tbody></table><p><strong>Итог:</strong> {{.Outcome}}</p>
{{end}}
<div class="signature"><p><span>Председатель ____________________</span><span>{{.ChairName}}</span></p><p><span>Секретарь ______________________</span><span>{{.SecretaryName}}</span></p></div>
</body></html>`
