package splitter

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testTool(t *testing.T) (string, string) {
	t.Helper()
	tool, err := exec.LookPath("tofu")
	if err != nil {
		t.Skip("OpenTofu is required for native state movement")
	}
	raw, err := exec.Command(tool, "version", "-json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Version string `json:"terraform_version"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return tool, v.Version
}

func writeTest(t *testing.T, path, source string) {
	t.Helper()
	if err := writePrivate(path, []byte(source)); err != nil {
		t.Fatal(err)
	}
}

func sourceFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "source")
	tool, version := testTool(t)
	writeTest(t, filepath.Join(root, "main.tf"), versions+`provider "spacelift" {}
variable "ancestor_space_ids" { default = [] }
resource "spacelift_context" "shared" { name = "shared" }
module "org" {
  source = "./org-source"
  inherited_contexts = { shared = spacelift_context.shared.id }
  ancestor_space_ids = var.ancestor_space_ids
}
`)
	writeTest(t, filepath.Join(root, "org-source", "main.tf"), versions+`variable "inherited_contexts" { type = map(string) }
variable "ancestor_space_ids" { type = list(string) }
resource "spacelift_space" "this" { name = "org" }
module "team_a" {
  source = "./team-a-source"
  parent_space_id = spacelift_space.this.id
  ancestor_space_ids = concat(var.ancestor_space_ids, [spacelift_space.this.id])
  inherited_contexts = var.inherited_contexts
}
module "team_b" {
  source = "./team-b-source"
  parent_space_id = spacelift_space.this.id
  ancestor_space_ids = concat(var.ancestor_space_ids, [spacelift_space.this.id])
  inherited_contexts = var.inherited_contexts
}
`)
	leaf := versions + `variable "parent_space_id" { type = string }
variable "ancestor_space_ids" { type = list(string) }
variable "inherited_contexts" { type = map(string) }
resource "spacelift_context" "unit" {
  for_each = toset(["one", "two"])
  name = each.key
  space_id = var.parent_space_id
}
`
	writeTest(t, filepath.Join(root, "org-source", "team-a-source", "main.tf"), leaf)
	writeTest(t, filepath.Join(root, "org-source", "team-b-source", "main.tf"), leaf)
	writeTest(t, filepath.Join(root, "ignored.tfstate"), "do not copy state")
	writeTest(t, filepath.Join(root, ".env"), "do not copy credentials")
	resource := func(module, typ, name string, keys ...string) any {
		var instances []any
		for _, key := range keys {
			instance := map[string]any{"schema_version": 0, "attributes": map[string]any{"id": name + "-" + key, "name": name}, "sensitive_attributes": []any{}}
			if key != "singleton" {
				instance["index_key"] = key
			}
			instances = append(instances, instance)
		}
		return map[string]any{"module": module, "mode": "managed", "type": typ, "name": name, "provider": `provider["registry.opentofu.org/spacelift-io/spacelift"]`, "instances": instances}
	}
	state := map[string]any{"version": 4, "terraform_version": version, "serial": 17, "lineage": "original-lineage", "outputs": map[string]any{}, "resources": []any{resource("", "spacelift_context", "shared", "singleton"), resource("module.org", "spacelift_space", "this", "singleton"), resource("module.org.module.team_a", "spacelift_context", "unit", "one", "two"), resource("module.org.module.team_b", "spacelift_context", "unit", "one", "two")}}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(base, "source.tfstate")
	writeTest(t, statePath, string(raw))
	spec := layout{SourceStackID: "existing-admin", SpaceID: "root", RoleID: "role-space-admin", Repository: "fixture-repo", ToolVersion: version, Groups: []group{{Name: "team_a", Module: "module.org.module.team_a"}, {Name: "team_b", Module: "module.org.module.team_b"}}}
	specPath := filepath.Join(base, "layout.json")
	if err := writeJSON(specPath, spec); err != nil {
		t.Fatal(err)
	}
	return root, statePath, specPath, tool
}

func TestPrepareNestedSiblingModules(t *testing.T) {
	root, statePath, specPath, tool := sourceFixture(t)
	original, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "prepared")
	report, err := prepare(root, statePath, specPath, out, tool, false)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.SourceInstances != 6 || report.ParentInstances != 2 || len(report.Groups) != 2 {
		t.Fatalf("unexpected report: %+v", report)
	}
	after, _ := os.ReadFile(statePath)
	if !bytes.Equal(original, after) {
		t.Fatal("modified source state")
	}
	backup, _ := os.ReadFile(filepath.Join(out, "private", "original.tfstate"))
	if !bytes.Equal(original, backup) {
		t.Fatal("backup differs")
	}
	for _, g := range report.Groups {
		if g.Instances != 2 {
			t.Fatalf("partition count %d", g.Instances)
		}
		if _, err := os.Stat(filepath.Join(out, "repo", "_split", g.Name, "org", g.Name, "main.tf")); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(out, "repo", "_split", g.Name, "main.tf"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "shared-singleton") || !strings.Contains(string(raw), "this-singleton") {
			t.Fatal("boundary input IDs were not carried into bootstrap configuration")
		}
	}
	parent, err := os.ReadFile(filepath.Join(out, "repo", "org-source", "main.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(parent), `module "team_`) {
		t.Fatal("selected module calls remain in parent")
	}
	exports, _ := os.ReadFile(filepath.Join(out, "repo", "org-source", "admin_split_outputs.tf"))
	if !strings.Contains(string(exports), "spacelift_space.this.id") || !strings.Contains(string(exports), "var.inherited_contexts") {
		t.Fatal("parent output lost dynamic shared inputs")
	}
	for _, name := range []string{"ignored.tfstate", ".env"} {
		if _, err := os.Stat(filepath.Join(out, "repo", name)); !os.IsNotExist(err) {
			t.Fatalf("copied private file %s", name)
		}
	}
	if _, err := prepare(root, statePath, specPath, out, tool, false); err == nil {
		t.Fatal("overwrote output directory")
	}
	info, err := os.Stat(filepath.Join(out, "private", "states", "parent.tfstate"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions %o", info.Mode().Perm())
	}
}

func TestPrepareNativeValidation(t *testing.T) {
	if os.Getenv("ADMIN_SPLITTER_VALIDATE_INTEGRATION") != "1" {
		t.Skip("set ADMIN_SPLITTER_VALIDATE_INTEGRATION=1 to initialize providers and validate generated roots")
	}
	root, statePath, specPath, tool := sourceFixture(t)
	report, err := prepare(root, statePath, specPath, filepath.Join(t.TempDir(), "validated"), tool, true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Validated {
		t.Fatal("configuration validation was not recorded")
	}
}

func TestRejectSensitiveBoundaryInput(t *testing.T) {
	root, statePath, specPath, tool := sourceFixture(t)
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	resources := state["resources"].([]any)
	instance := resources[0].(map[string]any)["instances"].([]any)[0].(map[string]any)
	instance["sensitive_attributes"] = []any{[]any{"id"}}
	if err := writeJSON(statePath, state); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "prepared")
	if _, err := prepare(root, statePath, specPath, out, tool, false); err == nil || !strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("expected sensitive input refusal, got %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("created outputs before rejecting sensitive inputs")
	}
}

func TestPartitionDetectsAttributeChange(t *testing.T) {
	original := &stateFile{Lineage: "source", Resources: []stateResource{{Module: "module.a", Type: "spacelift_context", Name: "x", Instances: []stateInstance{{Attributes: json.RawMessage(`{"id":"same","name":"original"}`)}}}}}
	part := &stateFile{Lineage: "child", Resources: []stateResource{{Module: "module.a", Type: "spacelift_context", Name: "x", Instances: []stateInstance{{Attributes: json.RawMessage(`{"name":"changed","id":"same"}`)}}}}}
	if err := verifyPartition(original, part, "module.a"); err == nil {
		t.Fatal("accepted a changed attribute")
	}
	part.Resources[0].Instances[0].Attributes = json.RawMessage("{ \"name\": \"original\", \"id\": \"same\" }")
	if err := verifyPartition(original, part, "module.a"); err != nil {
		t.Fatal(err)
	}
}

func TestRejectSensitiveTargetVariable(t *testing.T) {
	root, statePath, specPath, tool := sourceFixture(t)
	path := filepath.Join(root, "org-source", "team-a-source", "main.tf")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`variable "inherited_contexts" { type = map(string) }`), []byte("variable \"inherited_contexts\" {\n type = map(string)\n sensitive = true\n}"), 1)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = prepare(root, statePath, specPath, filepath.Join(t.TempDir(), "prepared"), tool, false)
	if err == nil || !strings.Contains(err.Error(), "declared sensitive") {
		t.Fatalf("expected sensitive target refusal, got %v", err)
	}
}

func TestVerifyPlans(t *testing.T) {
	dir := t.TempDir()
	prepared := filepath.Join(dir, "prepare.json")
	if err := writeJSON(prepared, prepareResult{Complete: true, Validated: true, ControllerCreates: []string{"spacelift_stack.admin_split_a"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, address, mode string
		actions             []string
		allow, want         bool
	}{{"unchanged", "module.a.spacelift_stack.unit", "managed", []string{"no-op"}, false, true}, {"data read", "data.spacelift_space.root", "data", []string{"read"}, false, true}, {"existing update", "module.a.spacelift_stack.unit", "managed", []string{"update"}, true, false}, {"replace", "module.a.spacelift_stack.unit", "managed", []string{"delete", "create"}, true, false}, {"known controller", "spacelift_stack.admin_split_a", "managed", []string{"create"}, true, true}, {"unknown controller", "spacelift_stack.admin_split_b", "managed", []string{"create"}, true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "plan.json")
			if err := writeJSON(path, map[string]any{"format_version": "1.2", "terraform_version": "1.11.4", "planned_values": map[string]any{}, "resource_changes": []any{map[string]any{"address": tc.address, "mode": tc.mode, "change": map[string]any{"actions": tc.actions}}}}); err != nil {
				t.Fatal(err)
			}
			err := verifyPlan(path, prepared, tc.allow)
			if (err == nil) != tc.want {
				t.Fatalf("got %v, want valid=%v", err, tc.want)
			}
		})
	}
}
