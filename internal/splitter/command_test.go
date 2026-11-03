package splitter

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoArgumentsShowsUsefulHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Execute(nil, &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("opening help failed: exit %d, %s", code, errOut.String())
	}
	for _, expected := range []string{"Usage:", "fetch", "inspect", "prepare", "check-source", "verify-plan", "--json", "manual"} {
		if !strings.Contains(out.String(), expected) {
			t.Errorf("help omits %q", expected)
		}
	}
}

func TestCommandsRejectPositionalArgumentsBeforeReadingInputs(t *testing.T) {
	for _, args := range [][]string{
		{"fetch", "--stack", "existing-admin", "--endpoint", "https://account.example", "--out", filepath.Join(t.TempDir(), "source"), "extra"},
		{"inspect", "--state", "missing-state", "extra"},
		{"prepare", "--state", "missing-state", "--config", "missing-config", "--source-stack", "missing-settings", "--out", "missing-out", "extra"},
		{"check-source", "--state", "missing-state", "--prepared", "missing-report", "extra"},
		{"verify-plan", "--plan", "missing-plan", "extra"},
	} {
		t.Run(args[0], func(t *testing.T) {
			var out bytes.Buffer
			if err := Run(args, &out); err == nil || !strings.Contains(err.Error(), "unexpected positional arguments") {
				t.Fatalf("positional argument was not rejected before input access: %v", err)
			}
			if out.Len() != 0 {
				t.Fatalf("invalid arguments emitted progress or success: %s", out.String())
			}
		})
	}
}

func TestExecuteUsesReadableErrorsOrExplicitJSON(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		json bool
	}{
		{"human", []string{"inspect"}, false},
		{"JSON", []string{"inspect", "--json"}, true},
		{"JSON unknown flag", []string{"inspect", "--json", "--unknown"}, true},
		{"explicit human", []string{"inspect", "--json=false"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := Execute(test.args, &out, &errOut); code != 1 || out.Len() != 0 {
				t.Fatalf("error did not produce only a failing stderr result: exit %d, %s", code, out.String())
			}
			if test.json {
				var result map[string]string
				decoder := json.NewDecoder(&errOut)
				if err := decoder.Decode(&result); err != nil || len(result) != 1 || result["error"] == "" {
					t.Fatalf("error is not the existing JSON schema: %v", err)
				}
				if err := decoder.Decode(&result); err != io.EOF {
					t.Fatal("JSON error included extra output")
				}
			} else if !strings.HasPrefix(errOut.String(), "Error: ") || strings.HasPrefix(errOut.String(), "{") {
				t.Fatalf("error was not readable: %s", errOut.String())
			}
		})
	}
}

func TestInspectHumanOutputAndJSONSchema(t *testing.T) {
	statePath, _, _, _ := sourceCheckFixture(t)
	var human bytes.Buffer
	if err := Run([]string{"inspect", "--state", statePath}, &human); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"OpenTofu version: 1.10.0", "Resource instances: 0", "Outputs: 1", "Next:", "--state"} {
		if !strings.Contains(human.String(), expected) {
			t.Errorf("human state summary omits %q", expected)
		}
	}
	if strings.Contains(human.String(), "private-state-value") || json.Valid(human.Bytes()) {
		t.Fatal("default output was JSON or exposed state values")
	}
	var machine bytes.Buffer
	if err := Run([]string{"inspect", "--state", statePath, "--json"}, &machine); err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(machine.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result) != 8 || string(result["tool_version"]) != `"1.10.0"` || string(result["outputs"]) != "1" || string(result["serial"]) != "7" {
		t.Fatalf("state JSON schema changed: %s", machine.String())
	}
}

func TestPrepareHumanStatusRequiresCompleteAndValidated(t *testing.T) {
	for _, test := range []struct {
		complete, validated bool
	}{
		{true, true}, {true, false}, {false, true}, {false, false},
	} {
		var out bytes.Buffer
		result := &prepareResult{
			Complete: test.complete, Validated: test.validated, SourceStackID: "existing-admin", WorkerPoolID: "original-private-pool",
			SourceInstances: 29, ParentInstances: 5, ToolVersion: "1.10.0",
			Groups: []groupResult{{Name: "team", Module: "module.team", Instances: 24}},
		}
		if err := displayPreparation(&out, result, "prepared"); err != nil {
			t.Fatal(err)
		}
		for _, expected := range []string{"original-private-pool", "29", "5", "module.team", "prepared/repo", "prepared/private/states"} {
			if !strings.Contains(out.String(), expected) {
				t.Errorf("preparation summary omits %q", expected)
			}
		}
		if test.complete && test.validated {
			if !strings.Contains(out.String(), "validated locally") || !strings.Contains(out.String(), "prepared/CUTOVER.md") || strings.Contains(out.String(), "NOT ready") {
				t.Fatal("validated preparation did not point to the manual cutover guide")
			}
		} else if !strings.Contains(out.String(), "NOT ready for cutover") || strings.Contains(out.String(), "follow the manual cutover steps") {
			t.Fatal("incomplete or diagnostic preparation was presented as ready for cutover")
		}
	}
}

func TestCheckSourceCommandHumanJSONAndFailure(t *testing.T) {
	statePath, preparedPath, raw, _ := sourceCheckFixture(t)
	var human bytes.Buffer
	if err := Run([]string{"check-source", "--state", statePath, "--prepared", preparedPath}, &human); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"matches the prepared snapshot", "frozen", "settings are unchanged"} {
		if !strings.Contains(human.String(), expected) {
			t.Errorf("source check summary omits %q", expected)
		}
	}
	var machine bytes.Buffer
	if err := Run([]string{"check-source", "--json", "--state", statePath, "--prepared", preparedPath}, &machine); err != nil {
		t.Fatal(err)
	}
	var result sourceCheckResult
	if err := json.Unmarshal(machine.Bytes(), &result); err != nil || !result.Matches || result.SourceStackID != "existing-admin" {
		t.Fatalf("source check JSON was invalid: %v", err)
	}
	writeTest(t, statePath, strings.Replace(string(raw), `"serial":7`, `"serial":8`, 1))
	var out, errOut bytes.Buffer
	if code := Execute([]string{"check-source", "--json", "--state", statePath, "--prepared", preparedPath}, &out, &errOut); code != 1 || out.Len() != 0 {
		t.Fatal("source mismatch emitted success or returned zero")
	}
	var failure map[string]string
	if err := json.Unmarshal(errOut.Bytes(), &failure); err != nil || !strings.Contains(failure["error"], "state changed") {
		t.Fatalf("source mismatch did not return a JSON error: %v", err)
	}
}

func TestVerifyPlanHumanAndJSON(t *testing.T) {
	plan := filepath.Join(t.TempDir(), "plan.json")
	writeTest(t, plan, `{"format_version":"1.2","terraform_version":"1.10.0","planned_values":{"root_module":{}},"resource_changes":[]}`)
	var human bytes.Buffer
	if err := Run([]string{"verify-plan", "--plan", plan}, &human); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human.String(), "no managed-resource changes") || !strings.Contains(human.String(), "review output changes") {
		t.Fatal("plan check omitted its result or next step")
	}
	var machine bytes.Buffer
	if err := Run([]string{"verify-plan", "--json", "--plan", plan}, &machine); err != nil || machine.String() != "{\"valid\":true}\n" {
		t.Fatalf("plan JSON schema changed: %s, %v", machine.String(), err)
	}
}
