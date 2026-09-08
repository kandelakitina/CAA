package main

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

type historyTestDB struct {
	denyCommittee bool
	reads         int
}

func (db *historyTestDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	noRows := cancellationRow(func(...any) error { return pgx.ErrNoRows })
	if strings.Contains(sql, "SELECT q.title") {
		if !strings.Contains(sql, "$2 <> 'committee'") {
			return cancellationRow(func(...any) error { return errors.New("missing question permission check") })
		}
		if args[0] != int64(1) || (db.denyCommittee && args[1] == "committee") {
			return noRows
		}
		return decisionTestRow{"Исторический вопрос"}
	}
	db.reads++
	if !strings.Contains(sql, "question_id = $1 AND ($2::BIGINT = 0 OR id = $2)") {
		return cancellationRow(func(...any) error { return errors.New("round ownership filter missing") })
	}
	if args[0] != int64(1) || (args[1] != int64(10) && args[1] != int64(0)) {
		return noRows
	}
	if strings.Contains(sql, "FROM internal_review_rounds") {
		return decisionTestRow{int64(10), "active", "", time.Now().AddDate(0, 0, 7), "Формулировка внутреннего раунда"}
	}
	return decisionTestRow{int64(10), "active", "", "Зафиксированное решение", time.Now().AddDate(0, 0, 7), 2}
}

func (db *historyTestDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	db.reads++
	now := time.Now()
	var rows []decisionTestRow
	switch {
	case strings.Contains(sql, "UNION ALL"):
		rows = []decisionTestRow{{int64(10), "internal", "cancelled", "", now, &now, now}, {int64(10), "committee", "completed", "approved", now, &now, now}}
	case strings.Contains(sql, "SELECT r.id, f.title"):
		rows = []decisionTestRow{{int64(101), "Материал", 2, "legal", "pending", int64(5), true}}
		if args[1] == true {
			rows = append(rows, decisionTestRow{int64(102), "Материал", 1, "legal", "superseded", int64(5), true})
		}
	case strings.Contains(sql, "SELECT v.id, v.requirement_id"):
		rows = []decisionTestRow{{int64(55), int64(102), "approved", "<script>Прежняя виза</script>", int64(7), "Согласующий", now, false, now, int64(0), "", now, 0, int64(0), ""}}
	case strings.Contains(sql, "SELECT frozen.question_file_id"):
		rows = []decisionTestRow{{int64(5), "Исключённый позднее материал", 1}}
	case strings.Contains(sql, "SELECT participant.id"):
		rows = []decisionTestRow{
			{int64(20), int64(7), "Имя на момент запуска", true, "voted", int64(80), "for", "За", "", now, false, now},
			{int64(20), int64(7), "Имя на момент запуска", true, "voted", int64(81), "against", "Прежний голос", "Материал · версия 1", now.Add(-time.Hour), true, now},
			{int64(21), int64(8), "Не ответивший участник", false, "pending", int64(0), "", "", "", now, false, now},
		}
	case strings.Contains(sql, "internal_visa_attachments"):
		rows = []decisionTestRow{{int64(55), int64(90), "Замечания.pdf", int64(100)}}
	case strings.Contains(sql, "committee_vote_attachments"):
		rows = []decisionTestRow{{int64(81), int64(91), "Отозванные замечания.pdf", int64(100)}}
	default:
		return nil, errors.New("unexpected history query: " + sql)
	}
	return &decisionTestRows{rows: rows}, nil
}

func TestRoundHistoryOwnershipAndAuthorization(t *testing.T) {
	for _, kind := range []string{"internal", "committee"} {
		for _, role := range []string{"secretary", "admin", "observer", "approver", "committee"} {
			db := &historyTestDB{denyCommittee: true}
			_, err := loadRoundHistoryPage(context.Background(), db, user{Role: role}, 1, 10, kind)
			if role == "committee" {
				if !errors.Is(err, pgx.ErrNoRows) || db.reads != 0 {
					t.Fatal("forbidden question must not reveal any round contents")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		}
		_, err := loadRoundHistoryPage(context.Background(), &historyTestDB{}, user{Role: "committee"}, 1, 10, kind)
		if err != nil {
			t.Fatal("Committee must see history of an accessible question", err)
		}
		_, err = loadRoundHistoryPage(context.Background(), &historyTestDB{}, user{Role: "secretary"}, 1, 999, kind)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal("foreign or missing round must not fall back to the latest round")
		}
	}
	db := &historyTestDB{}
	if _, err := loadRoundHistoryPage(context.Background(), db, user{}, 1, 10, "forged"); !errors.Is(err, pgx.ErrNoRows) || db.reads != 0 {
		t.Fatal("invalid kind must not be queried")
	}
}

func TestSelectedInternalRoundContainsSupersededVersions(t *testing.T) {
	db := &historyTestDB{}
	view, err := loadInternalReviewRound(context.Background(), db, 1, "draft", user{ID: 7, Role: "approver", InternalService: "legal"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Items) != 2 || !view.Items[1].Superseded || !view.Items[1].HasVisa || len(view.Items[1].Visa.Attachments) != 1 {
		t.Fatal("old version, visa and attachment must remain visible")
	}
	if view.ProgressLabel != "0 из 1" {
		t.Fatal("superseded assignments must not affect final progress")
	}
	for _, item := range view.Items {
		if item.CanRespond || item.FilePaused || item.Visa.CanWithdraw {
			t.Fatal("selected round must be read-only and ignore current pending uploads")
		}
	}
	latest, err := loadInternalReviewRound(context.Background(), db, 1, "internal_review", user{}, 0)
	if err != nil || len(latest.Items) != 1 {
		t.Fatal("current card must retain its existing latest-version view", err)
	}
}

func TestSelectedCommitteeRoundPreservesRosterAndWithdrawals(t *testing.T) {
	view, err := loadCommitteeVoteRound(context.Background(), &historyTestDB{}, 1, "ready_for_committee", user{ID: 7, Role: "secretary"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if view.CanStart || view.CanManage || view.CanClose || view.CanVote {
		t.Fatal("selected round must never offer management")
	}
	if view.DecisionText != "Зафиксированное решение" || len(view.Files) != 1 || view.Files[0].VersionNo != 1 || view.ProgressLabel != "1 из 2" {
		t.Fatal("frozen decision, bundle or roster lost")
	}
	member := view.Participants[0]
	if member.Name != "Имя на момент запуска" || !member.IsChair || member.Vote.CanWithdraw || len(member.History) != 1 || member.History[0].WithdrawnLabel == "" || len(member.History[0].Attachments) != 1 {
		t.Fatal("roster or withdrawal history incorrect")
	}
}

func TestRoundHistoryTemplateReadOnly(t *testing.T) {
	tmpl, err := template.ParseFiles("templates/round-history.html", "templates/navigation.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"internal", "committee"} {
		page, err := loadRoundHistoryPage(context.Background(), &historyTestDB{}, user{ID: 7, Role: "secretary"}, 1, 10, kind)
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := tmpl.Execute(&output, gin.H{"Title": "История", "User": user{}, "Page": page}); err != nil {
			t.Fatal(err)
		}
		body := output.String()
		if strings.Contains(body, "<form") || strings.Contains(body, "<script>") {
			t.Fatal("history must not contain mutation forms or unescaped comments")
		}
		if !strings.Contains(body, "/files/5/versions/1/download") {
			t.Fatal("historical file download missing")
		}
		if kind == "internal" && !strings.Contains(body, "&lt;script&gt;Прежняя виза&lt;/script&gt;") {
			t.Fatal("historical visa comment missing")
		}
	}
}
