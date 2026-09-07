package main

import (
	"strings"
	"testing"
	"time"
)

func TestValidateInternalDecision(t *testing.T) {
	for _, test := range []struct {
		name     string
		decision string
		comment  string
		wantErr  bool
	}{
		{name: "approved", decision: "approved"},
		{name: "approved with comments", decision: "approved_with_comments", comment: "Исправить нумерацию"},
		{name: "rejected", decision: "rejected", comment: "Критическое замечание"},
		{name: "outside competence", decision: "no_comments_without_review"},
		{name: "missing recommended comment", decision: "approved_with_comments", wantErr: true},
		{name: "missing rejection comment", decision: "rejected", wantErr: true},
		{name: "unknown", decision: "abstain", wantErr: true},
		{name: "long comment", decision: "approved", comment: strings.Repeat("я", 5001), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateInternalDecision(test.decision, test.comment)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestValidateInternalReviewStart(t *testing.T) {
	today := time.Date(2026, 9, 7, 12, 0, 0, 0, time.Local)
	deadline := time.Date(2026, 9, 8, 0, 0, 0, 0, time.Local)
	allServices := map[string]int{"legal": 1, "finance": 2, "construction": 1, "security": 1}
	for _, test := range []struct {
		name     string
		question string
		deadline time.Time
		files    []reviewStartFile
		services map[string]int
		pending  int
		wantErr  bool
	}{
		{name: "ordinary", question: "budget", deadline: deadline, files: []reviewStartFile{{ID: 1, VersionNo: 1, Category: "other"}}, services: allServices},
		{name: "contract bundle", question: "transaction", deadline: deadline, files: []reviewStartFile{{Category: "contract"}, {Category: "terms_summary"}}, services: allServices},
		{name: "lna bundle", question: "internal_document", deadline: deadline, files: []reviewStartFile{{Category: "lna_draft"}}, services: allServices},
		{name: "empty bundle", question: "budget", deadline: deadline, services: allServices, wantErr: true},
		{name: "pending upload", question: "budget", deadline: deadline, files: []reviewStartFile{{Category: "other"}}, services: allServices, pending: 1, wantErr: true},
		{name: "missing contract summary", question: "transaction", deadline: deadline, files: []reviewStartFile{{Category: "contract"}}, services: allServices, wantErr: true},
		{name: "missing lna draft", question: "internal_document", deadline: deadline, files: []reviewStartFile{{Category: "other"}}, services: allServices, wantErr: true},
		{name: "missing service", question: "budget", deadline: deadline, files: []reviewStartFile{{Category: "other"}}, services: map[string]int{"legal": 1}, wantErr: true},
		{name: "past deadline", question: "budget", deadline: today.AddDate(0, 0, -1), files: []reviewStartFile{{Category: "other"}}, services: allServices, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateInternalReviewStart(test.question, test.deadline, today, test.files, test.services, test.pending)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, test.wantErr)
			}
		})
	}
}
