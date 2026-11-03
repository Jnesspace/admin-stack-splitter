package splitter

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

func migrationKitFixture(t *testing.T) (string, string, string, string, map[string]map[string]any) {
	t.Helper()
	tool, version := testTool(t)
	base := t.TempDir()
	config := filepath.Join(base, "source")
	var resources []any
	expected := map[string]map[string]any{}
	err := filepath.WalkDir("testdata/migration-kit", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, err := filepath.Rel("testdata/migration-kit", path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		writeTest(t, filepath.Join(config, rel), string(raw))
		file, diags := hclsyntax.ParseConfig(raw, path, hcl.InitialPos)
		if diags.HasErrors() {
			t.Fatal(diags.Error())
		}
		module := ""
		if dir := filepath.Dir(rel); dir != "." {
			module = "module." + strings.ReplaceAll(filepath.ToSlash(dir), "/", ".module.")
		}
		for _, block := range file.Body.(*hclsyntax.Body).Blocks {
			if block.Type != "resource" {
				continue
			}
			address := strings.TrimPrefix(module+"."+strings.Join(block.Labels, "."), ".")
			attrs := map[string]any{"id": "fixture-" + strings.ReplaceAll(address, ".", "-"), "name": block.Labels[1]}
			expected[address] = attrs
			resources = append(resources, map[string]any{
				"module": module, "mode": "managed", "type": block.Labels[0], "name": block.Labels[1],
				"provider":  `provider["registry.opentofu.org/spacelift-io/spacelift"]`,
				"instances": []any{map[string]any{"schema_version": 0, "attributes": attrs, "sensitive_attributes": []any{}}},
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(base, "source.tfstate")
	state := map[string]any{"version": 4, "terraform_version": version, "serial": 9, "lineage": "migration-kit-fixture", "outputs": map[string]any{}, "resources": resources}
	if err := writeJSON(statePath, state); err != nil {
		t.Fatal(err)
	}
	layoutPath := filepath.Join(base, "layout.json")
	spec := layout{SourceStackID: "existing-admin", SpaceID: "root", RoleID: "role-space-admin", Repository: "migration-admin", VCSProvider: "GITHUB", Groups: []group{
		{Name: "team_a", Module: "module.organization.module.team_a"},
		{Name: "team_b", Module: "module.organization.module.team_b"},
	}}
	if err := writeJSON(layoutPath, spec); err != nil {
		t.Fatal(err)
	}
	return config, statePath, layoutPath, tool, expected
}

func TestPrepareMigrationKitGeneratedTree(t *testing.T) {
	config, snapshot, layoutPath, tool, expected := migrationKitFixture(t)
	out := filepath.Join(t.TempDir(), "prepared")
	report, err := prepare(config, snapshot, layoutPath, out, tool, false)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.Validated || report.SourceInstances != len(expected) || len(report.Groups) != 2 {
		t.Fatalf("unexpected preparation result: %+v", report)
	}
	actual := map[string]map[string]any{}
	for _, name := range []string{"parent", "team_a", "team_b"} {
		state, _, err := readState(filepath.Join(out, "private", "states", name+".tfstate"))
		if err != nil {
			t.Fatal(err)
		}
		if (name == "parent") != (state.Lineage == "migration-kit-fixture") {
			t.Fatalf("unexpected %s state lineage %q", name, state.Lineage)
		}
		for _, resource := range state.Resources {
			address := strings.TrimPrefix(resource.Module+"."+resource.Type+"."+resource.Name, ".")
			if len(resource.Instances) != 1 || actual[address] != nil {
				t.Fatalf("resource %s is duplicated or has unexpected instance count", address)
			}
			if name != "parent" && resource.Module != "module.organization.module."+name {
				t.Fatalf("%s owns resource outside its subtree: %s", name, address)
			}
			if name == "parent" && strings.Contains(resource.Module, ".module.team_") {
				t.Fatalf("parent still owns a selected resource: %s", address)
			}
			var attrs map[string]any
			if err := json.Unmarshal(resource.Instances[0].Attributes, &attrs); err != nil {
				t.Fatal(err)
			}
			actual[address] = attrs
		}
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatal("split states lost, duplicated, moved or changed original resources")
	}
	for _, name := range []string{"team_a", "team_b"} {
		childRoot := filepath.Join(out, "repo", "_split", name)
		raw, err := os.ReadFile(filepath.Join(childRoot, "main.tf"))
		if err != nil {
			t.Fatal(err)
		}
		file, diags := hclsyntax.ParseConfig(raw, "main.tf", hcl.InitialPos)
		if diags.HasErrors() {
			t.Fatal(diags.Error())
		}
		providerFound := false
		payloadFound := false
		for _, block := range file.Body.(*hclsyntax.Body).Blocks {
			if block.Type == "provider" && block.Labels[0] == "spacelift" {
				providerFound = true
			}
			if block.Type != "variable" || block.Labels[0] != "split_inputs" {
				continue
			}
			payloadFound = true
			value, diags := block.Body.Attributes["default"].Expr.Value(nil)
			if diags.HasErrors() {
				t.Fatal(diags.Error())
			}
			var payload struct {
				Inputs struct {
					Parent    string            `json:"parent_space_id"`
					Ancestors []string          `json:"ancestor_space_ids"`
					Contexts  map[string]string `json:"inherited_contexts"`
				} `json:"inputs"`
			}
			if err := json.Unmarshal([]byte(value.AsString()), &payload); err != nil {
				t.Fatal(err)
			}
			parentID := expected["module.organization.spacelift_space.this"]["id"].(string)
			contexts := map[string]string{"global": expected["spacelift_context.global"]["id"].(string), "shared": expected["spacelift_context.shared"]["id"].(string)}
			if payload.Inputs.Parent != parentID || !reflect.DeepEqual(payload.Inputs.Ancestors, []string{"root", parentID}) || !reflect.DeepEqual(payload.Inputs.Contexts, contexts) {
				t.Fatalf("%s lost inherited space/context inputs: %+v", name, payload.Inputs)
			}
		}
		if !providerFound {
			t.Fatalf("%s root did not inherit the Spacelift provider", name)
		}
		if !payloadFound {
			t.Fatalf("%s root has no bootstrap inputs", name)
		}
		original, err := os.ReadFile(filepath.Join(config, "organization", name, "stack_api.tf"))
		if err != nil {
			t.Fatal(err)
		}
		copied, err := os.ReadFile(filepath.Join(childRoot, "organization", name, "stack_api.tf"))
		if err != nil {
			t.Fatal(err)
		}
		if string(original) != string(copied) {
			t.Fatalf("%s stack configuration changed", name)
		}
	}
}

func TestPrepareMigrationKitNativeValidation(t *testing.T) {
	if os.Getenv("ADMIN_SPLITTER_VALIDATE_INTEGRATION") != "1" {
		t.Skip("set ADMIN_SPLITTER_VALIDATE_INTEGRATION=1 to initialize providers")
	}
	config, snapshot, layoutPath, tool, _ := migrationKitFixture(t)
	report, err := prepare(config, snapshot, layoutPath, filepath.Join(t.TempDir(), "prepared"), tool, true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Validated {
		t.Fatal("migration-kit parent and child roots were not validated")
	}
}
