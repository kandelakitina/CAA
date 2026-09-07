package main

import (
	"testing"
	"time"
)

func TestValidateQuestionInput(t *testing.T) {
	today := time.Date(2026, 9, 7, 12, 0, 0, 0, time.Local)
	tests := []struct {
		name    string
		input   questionInput
		wantErr bool
	}{
		{
			name: "ordinary question",
			input: questionInput{
				QuestionType: "budget", Title: "Бюджет на 2027 год",
				DecisionText: "Рекомендовать утвердить бюджет.", InternalDeadline: "2026-09-08",
			},
		},
		{
			name: "purchase contract",
			input: questionInput{
				QuestionType: "transaction", TransactionType: "purchase", Title: "Договор",
				DecisionText: "Одобрить договор.", InternalDeadline: "2026-09-07",
				Counterparty: "ООО Контрагент", Amount: "1000000,50", Currency: "rub",
			},
		},
		{
			name: "missing decision",
			input: questionInput{
				QuestionType: "kpi", Title: "КПЭ", InternalDeadline: "2026-09-08",
			},
			wantErr: true,
		},
		{
			name: "past deadline",
			input: questionInput{
				QuestionType: "other", Title: "Вопрос", DecisionText: "Принять.", InternalDeadline: "2026-09-06",
			},
			wantErr: true,
		},
		{
			name: "transaction without details",
			input: questionInput{
				QuestionType: "transaction", Title: "Договор", DecisionText: "Одобрить.", InternalDeadline: "2026-09-08",
			},
			wantErr: true,
		},
		{
			name: "zero amount",
			input: questionInput{
				QuestionType: "transaction", TransactionType: "financial", Title: "Заём",
				DecisionText: "Одобрить.", InternalDeadline: "2026-09-08",
				Counterparty: "Компания", Amount: "00.00", Currency: "RUB",
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, _, err := validateQuestionInput(test.input, today)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, test.wantErr)
			}
			if err == nil && test.input.QuestionType == "transaction" && got.Currency != "RUB" {
				t.Fatalf("currency=%q, want RUB", got.Currency)
			}
		})
	}
}

func TestInternalServices(t *testing.T) {
	for _, service := range []string{"legal", "finance", "construction", "security"} {
		if !validInternalService(service) {
			t.Fatalf("service %q must be valid", service)
		}
		if internalServiceLabel(service) == "" {
			t.Fatalf("service %q must have a label", service)
		}
	}
	if validInternalService("") || validInternalService("production") {
		t.Fatal("unknown service must not be valid")
	}
}
