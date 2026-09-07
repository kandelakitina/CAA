package main

import (
	"testing"
	"time"
)

func TestBuildRevisionPlan(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	sources := map[string]revisionSourceVisa{}
	for _, service := range requiredInternalServices {
		decision := "approved"
		if service == "legal" {
			decision = "rejected"
		}
		sources[revisionVisaKey(10, service)] = revisionSourceVisa{
			VisaID: 1, FileID: 10, VersionNo: 1, Service: service,
			Decision: decision, DecidedBy: 5, DecidedAt: now,
		}
	}
	files := []revisionFile{{ID: 10, Title: "Договор", CurrentVersion: 2}}
	selected := map[string]bool{revisionVisaKey(10, "finance"): true}
	plan, fresh, carried, err := buildRevisionPlan(files, sources, selected)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Ready || fresh != 2 || carried != 2 {
		t.Fatalf("ready=%v fresh=%d carried=%d", plan.Ready, fresh, carried)
	}
}

func TestBuildRevisionPlanRequiresNewRejectedVersion(t *testing.T) {
	sources := map[string]revisionSourceVisa{}
	for _, service := range requiredInternalServices {
		decision := "approved"
		if service == "security" {
			decision = "rejected"
		}
		sources[revisionVisaKey(3, service)] = revisionSourceVisa{FileID: 3, VersionNo: 4, Service: service, Decision: decision}
	}
	plan, _, _, err := buildRevisionPlan([]revisionFile{{ID: 3, CurrentVersion: 4}}, sources, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Ready || plan.BlockReason == "" {
		t.Fatal("unchanged rejected file must block a new round")
	}
}

func TestBuildRevisionPlanNewFileRequiresAllServices(t *testing.T) {
	plan, fresh, carried, err := buildRevisionPlan([]revisionFile{{ID: 7, CurrentVersion: 1}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Ready || fresh != 4 || carried != 0 || !plan.Files[0].IsNew {
		t.Fatalf("unexpected plan: ready=%v fresh=%d carried=%d new=%v", plan.Ready, fresh, carried, plan.Files[0].IsNew)
	}
}

func TestBuildRevisionPlanRejectsForgedSelection(t *testing.T) {
	_, _, _, err := buildRevisionPlan(
		[]revisionFile{{ID: 1, CurrentVersion: 2}},
		map[string]revisionSourceVisa{revisionVisaKey(1, "legal"): {FileID: 1, VersionNo: 1, Service: "legal", Decision: "approved"}},
		map[string]bool{"999:security": true},
	)
	if err == nil {
		t.Fatal("forged selection must be rejected")
	}
}
