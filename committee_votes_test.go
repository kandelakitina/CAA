package main

import (
	"strings"
	"testing"
)

func TestValidateCommitteeDecision(t *testing.T) {
	for _, test := range []struct {
		name     string
		decision string
		comment  string
		wantErr  bool
	}{
		{name: "for", decision: "for"},
		{name: "for with comments", decision: "for_with_comments", comment: "Уточнить срок"},
		{name: "against", decision: "against", comment: "Не хватает расчётов"},
		{name: "abstain", decision: "abstain", comment: "Конфликт интересов"},
		{name: "missing comment for with comments", decision: "for_with_comments", wantErr: true},
		{name: "missing comment against", decision: "against", wantErr: true},
		{name: "missing comment abstain", decision: "abstain", wantErr: true},
		{name: "unknown", decision: "approved", wantErr: true},
		{name: "long comment", decision: "for", comment: strings.Repeat("я", 5001), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateCommitteeDecision(test.decision, test.comment)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestCalculateCommitteeOutcome(t *testing.T) {
	for _, test := range []struct {
		name      string
		roster    int
		responded int
		support   int
		quorum    bool
		outcome   string
	}{
		{name: "no quorum", roster: 5, responded: 2, support: 2, outcome: "no_quorum"},
		{name: "approved", roster: 5, responded: 3, support: 2, quorum: true, outcome: "approved"},
		{name: "rejected", roster: 4, responded: 3, support: 1, quorum: true, outcome: "rejected"},
		{name: "tie is rejected", roster: 3, responded: 2, support: 1, quorum: true, outcome: "rejected"},
		{name: "abstention counts as response", roster: 3, responded: 3, support: 2, quorum: true, outcome: "approved"},
	} {
		t.Run(test.name, func(t *testing.T) {
			quorum, outcome := calculateCommitteeOutcome(test.roster, test.responded, test.support)
			if quorum != test.quorum || outcome != test.outcome {
				t.Fatalf("got quorum=%v outcome=%q, want quorum=%v outcome=%q", quorum, outcome, test.quorum, test.outcome)
			}
		})
	}
}
