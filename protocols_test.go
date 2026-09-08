package main

import (
	"strings"
	"testing"
)

func TestParseProtocolSelections(t *testing.T) {
	for _, test := range []struct {
		name    string
		form    map[string][]string
		wantIDs []int64
		wantErr bool
	}{
		{
			name: "sorts by agenda order",
			form: map[string][]string{
				"question_id": {"20", "10"}, "order_20": {"2"}, "order_10": {"1"},
			},
			wantIDs: []int64{10, 20},
		},
		{name: "empty", form: map[string][]string{}, wantErr: true},
		{name: "duplicate question", form: map[string][]string{"question_id": {"10", "10"}, "order_10": {"1"}}, wantErr: true},
		{name: "duplicate order", form: map[string][]string{"question_id": {"10", "20"}, "order_10": {"1"}, "order_20": {"1"}}, wantErr: true},
		{name: "gap in order", form: map[string][]string{"question_id": {"10", "20"}, "order_10": {"1"}, "order_20": {"3"}}, wantErr: true},
		{name: "invalid id", form: map[string][]string{"question_id": {"zero"}, "order_zero": {"1"}}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseProtocolSelections(test.form)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, test.wantErr)
			}
			if err != nil {
				return
			}
			if len(got) != len(test.wantIDs) {
				t.Fatalf("got %d selections, want %d", len(got), len(test.wantIDs))
			}
			for index, wantID := range test.wantIDs {
				if got[index].QuestionID != wantID || got[index].Order != index+1 {
					t.Fatalf("selection %d = %+v, want question %d at order %d", index, got[index], wantID, index+1)
				}
			}
		})
	}
}

func TestProtocolNumber(t *testing.T) {
	if got := protocolNumber(12, 2026); got != "12/2026" {
		t.Fatalf("number=%q", got)
	}
}

func TestRenderProtocolWordEscapesUserText(t *testing.T) {
	view := protocolView{
		Number: "1/2026", DateLabel: "08.09.2026", TimeLabel: "12:00",
		Place: protocolPlace, ChairName: "Председатель", SecretaryName: "Секретарь",
		Questions: []protocolQuestionView{{
			Order: 1, Title: `<script>alert("x")</script>`, DecisionText: "Одобрить",
			Outcome:      "Решение принято",
			Participants: []protocolParticipantView{{Name: "Участник", Responded: true, Decision: "За", AttachmentCount: 2}},
		}},
	}
	content, err := renderProtocolWord(view)
	if err != nil {
		t.Fatal(err)
	}
	result := string(content)
	if strings.Contains(result, "<script>") {
		t.Fatal("Word export must escape user-controlled HTML")
	}
	if !strings.Contains(result, "Приложено файлов: 2") || !strings.Contains(result, "1/2026") {
		t.Fatalf("Word export misses protocol data: %s", result)
	}
}
