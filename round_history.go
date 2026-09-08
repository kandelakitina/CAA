package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

type roundHistoryDB interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type roundHistoryItem struct {
	ID                                                                       int64
	Kind, KindLabel, StatusLabel, StartedLabel, FinishedLabel, DeadlineLabel string
}

type roundHistoryPage struct {
	QuestionID, RoundID int64
	QuestionTitle, Kind string
	Rounds              []roundHistoryItem
	Internal            internalReviewView
	Committee           committeeVoteView
}

func validRoundKind(kind string) bool { return kind == "internal" || kind == "committee" }

func readOnlyInternalReview(view internalReviewView) internalReviewView {
	view.CanStart, view.CanManage = false, false
	for i := range view.Items {
		view.Items[i].CanRespond, view.Items[i].FilePaused = false, false
		view.Items[i].Visa.CanWithdraw = false
		for j := range view.Items[i].History {
			view.Items[i].History[j].CanWithdraw = false
		}
	}
	return view
}

func readOnlyCommitteeVote(view committeeVoteView) committeeVoteView {
	view.CanStart, view.CanManage, view.CanClose, view.CanVote = false, false, false, false
	for i := range view.Participants {
		view.Participants[i].CanVote, view.Participants[i].Vote.CanWithdraw = false, false
		for j := range view.Participants[i].History {
			view.Participants[i].History[j].CanWithdraw = false
		}
	}
	return view
}

func loadRoundHistory(ctx context.Context, db roundHistoryDB, questionID int64) ([]roundHistoryItem, error) {
	rows, err := db.Query(ctx, `SELECT id, kind, status, outcome, started_at, finished_at, deadline FROM (
		SELECT id, 'internal' AS kind, status, COALESCE(outcome, '') AS outcome, started_at,
		       COALESCE(completed_at, cancelled_at) AS finished_at, deadline
		FROM internal_review_rounds WHERE question_id = $1
		UNION ALL
		SELECT id, 'committee' AS kind, status, COALESCE(outcome, ''), started_at,
		       COALESCE(completed_at, cancelled_at), deadline
		FROM committee_vote_rounds WHERE question_id = $1
	) rounds ORDER BY started_at DESC, kind, id DESC`, questionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []roundHistoryItem
	for rows.Next() {
		var item roundHistoryItem
		var status, outcome string
		var started, deadline time.Time
		var finished *time.Time
		if err := rows.Scan(&item.ID, &item.Kind, &status, &outcome, &started, &finished, &deadline); err != nil {
			return nil, err
		}
		item.KindLabel = "Внутреннее согласование"
		if item.Kind == "committee" {
			item.KindLabel = "Голосование Комитета"
		}
		switch status {
		case "active":
			item.StatusLabel = "Открыт"
		case "cancelled":
			item.StatusLabel = "Отменён"
		default:
			item.StatusLabel = questionStatusLabel(outcome)
		}
		item.StartedLabel, item.DeadlineLabel = started.Format("02.01.2006 15:04"), deadline.Format("02.01.2006")
		if finished != nil {
			item.FinishedLabel = finished.Format("02.01.2006 15:04")
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// Authorize the question before reading either the round list or round contents.
// The selected loader also requires the round to belong to this question.
func loadRoundHistoryPage(ctx context.Context, db roundHistoryDB, usr user, questionID, roundID int64, kind string) (roundHistoryPage, error) {
	page := roundHistoryPage{QuestionID: questionID, RoundID: roundID, Kind: kind}
	if !validRoundKind(kind) || questionID < 1 || roundID < 1 {
		return page, pgx.ErrNoRows
	}
	err := db.QueryRow(ctx, `SELECT q.title FROM questions q WHERE q.id = $1
		AND ($2 <> 'committee' OR q.status IN ('committee_voting', 'approved', 'rejected', 'no_quorum')
		OR (q.status = 'cancelled' AND EXISTS (SELECT 1 FROM committee_vote_rounds WHERE question_id = q.id)))`, questionID, usr.Role).Scan(&page.QuestionTitle)
	if err != nil {
		return page, err
	}
	if kind == "internal" {
		page.Internal, err = loadInternalReviewRound(ctx, db, questionID, "", usr, roundID)
	} else {
		page.Committee, err = loadCommitteeVoteRound(ctx, db, questionID, "", usr, roundID)
	}
	if err != nil {
		return page, err
	}
	page.Rounds, err = loadRoundHistory(ctx, db, questionID)
	return page, err
}

func (app *application) showRoundHistory(c *gin.Context) {
	questionID, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	roundID, ok := parsePositiveID(c, "roundID", "Некорректный идентификатор раунда")
	if !ok {
		return
	}
	kind := c.Param("kind")
	if !validRoundKind(kind) {
		c.String(http.StatusNotFound, "Раунд не найден")
		return
	}
	page, err := loadRoundHistoryPage(c.Request.Context(), app.db, c.MustGet("user").(user), questionID, roundID, kind)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Вопрос или раунд не найден")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить историю раунда")
		return
	}
	c.HTML(http.StatusOK, "round-history.html", gin.H{"Title": "История раунда", "User": c.MustGet("user").(user),
		"CSRFToken": app.templateCSRF(c), "Page": page})
}
