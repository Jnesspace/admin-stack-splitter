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

func TestVerifyPlanMatchesPreparation(t *testing.T) {
	dir := t.TempDir()
	prepared := filepath.Join(dir, "prepare.json")
	if err := writeJSON(prepared, prepareResult{Complete: true, Validated: true, ToolVersion: "1.10.6", ControllerCreates: []string{"spacelift_stack.admin_split_a", "spacelift_stack.admin_split_b"}}); err != nil {
		t.Fatal(err)
	}
	create := func(address string) string {
		return `{"address":"` + address + `","mode":"managed","change":{"actions":["create"]}}`
	}
	for _, tc := range []struct {
		name, version, changes string
		allow                  bool
		want                   string
	}{
		{"all recorded creates", "1.10.6", create("spacelift_stack.admin_split_a") + "," + create("spacelift_stack.admin_split_b"), true, ""},
		{"missing recorded create", "1.10.6", create("spacelift_stack.admin_split_a"), true, "does not create recorded controller addition spacelift_stack.admin_split_b"},
		{"duplicate recorded create", "1.10.6", create("spacelift_stack.admin_split_a") + "," + create("spacelift_stack.admin_split_a"), true, "plan changes existing resource spacelift_stack.admin_split_a"},
		{"parent version mismatch", "1.11.0", create("spacelift_stack.admin_split_a") + "," + create("spacelift_stack.admin_split_b"), true, "preparation recorded 1.10.6"},
		{"child version mismatch", "1.11.0", "", false, "preparation recorded 1.10.6"},
		{"child version match", "1.10.6", "", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "plan.json")
			writeTest(t, path, `{"format_version":"1.2","terraform_version":"`+tc.version+`","planned_values":{},"resource_changes":[`+tc.changes+`]}`)
			err := verifyPlan(path, prepared, tc.allow)
			if tc.want == "" && err != nil {
				t.Fatalf("rejected a matching plan: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}
