package splitter

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyRejectsStateAndIncompletePlans(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
	}{
		{"state show", `{"format_version":"1.0","terraform_version":"1.10.6","values":{"root_module":{}}}`},
		{"missing plan values", `{"format_version":"1.2","terraform_version":"1.10.6","resource_changes":[]}`},
		{"null plan values", `{"format_version":"1.2","terraform_version":"1.10.6","planned_values":null}`},
		{"invalid plan values", `{"format_version":"1.2","terraform_version":"1.10.6","planned_values":[]}`},
		{"errored plan", `{"format_version":"1.2","terraform_version":"1.10.6","planned_values":{},"errored":true}`},
		{"incomplete plan", `{"format_version":"1.2","terraform_version":"1.10.6","planned_values":{},"complete":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "plan.json")
			writeTest(t, path, tc.json)
			if err := verifyPlan(path, "", false); err == nil {
				t.Fatal("accepted a non-plan or incomplete plan")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "empty-plan.json")
	writeTest(t, path, `{"format_version":"1.2","terraform_version":"1.10.6","planned_values":{}}`)
	if err := verifyPlan(path, "", false); err != nil {
		t.Fatalf("rejected a valid empty plan: %v", err)
	}
}

func TestVerifyPlanDoesNotEchoSensitiveValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	secret := "private-plan-value"
	writeTest(t, path, `{"format_version":"1.2","terraform_version":"1.10.6","planned_values":{},"resource_changes":[{"address":"spacelift_environment_variable.token","mode":"managed","change":{"actions":["update"],"after":{"value":"`+secret+`"}}}]}`)
	err := verifyPlan(path, "", false)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unexpected plan error: %v", err)
	}
}
