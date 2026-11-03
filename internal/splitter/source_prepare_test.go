package splitter

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

func sourcePrepareSettings(t *testing.T, version string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"stack": map[string]any{
		"id": "existing-admin", "name": "Existing admin", "space": "root",
		"repository": "migration-admin", "namespace": "", "branch": "main", "provider": "SPACELIFT",
		"projectRoot": "admin", "runnerImage": "registry.example.com/custom-runner:stable",
		"workerPool":     map[string]any{"id": "original-private-pool"},
		"vcsIntegration": map[string]any{"id": "migration-admin", "isDefault": false},
		"roleBindings": []any{map[string]any{
			"role": map[string]any{"id": "role-space-admin", "slug": "space-admin"}, "space": map[string]any{"id": "root"},
		}},
		"vendorConfig": map[string]any{"__typename": "StackConfigVendorOpenTofu", "openTofuWorkflowTool": "OPENTOFU", "version": version},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPrepareFromSourceStackSettings(t *testing.T) {
	for _, mode := range []string{"MCP file CLI", "GraphQL direct"} {
		t.Run(mode, func(t *testing.T) {
			config, snapshot, _, tool, _ := migrationKitFixture(t)
			state, _, err := readState(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			settings := sourcePrepareSettings(t, state.TerraformVersion)
			out := filepath.Join(t.TempDir(), "prepared")
			var report *prepareResult
			if mode == "MCP file CLI" {
				settingsPath := filepath.Join(t.TempDir(), "source-stack.json")
				writeTest(t, settingsPath, string(settings))
				var result bytes.Buffer
				if err := Run([]string{"prepare", "--config", config, "--state", snapshot, "--source-stack", settingsPath, "--out", out, "--tool", tool, "--validate=false"}, &result); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(result.Bytes(), &report); err != nil {
					t.Fatal(err)
				}
			} else {
				settings = append(append([]byte(`{"data":`), settings...), '}')
				report, err = prepareFromStack(config, snapshot, settings, "", out, tool, false)
				if err != nil {
					t.Fatal(err)
				}
			}
			if !report.Complete || report.SourceStackID != "existing-admin" || report.WorkerPoolID != "original-private-pool" || report.RunnerImage != "registry.example.com/custom-runner:stable" || report.ToolVersion != state.TerraformVersion {
				t.Fatalf("source runtime settings were not retained: %+v", report)
			}
			if report.SourceInstances != 29 || report.ParentInstances != 5 || len(report.Groups) != 2 || report.Groups[0].Name != "organization__team_a" || report.Groups[1].Name != "organization__team_b" {
				t.Fatalf("unexpected automatic groups: %+v", report)
			}
			raw, err := os.ReadFile(filepath.Join(out, "repo", "admin_split_controller.tf"))
			if err != nil {
				t.Fatal(err)
			}
			file, diags := hclsyntax.ParseConfig(raw, "admin_split_controller.tf", hcl.InitialPos)
			if diags.HasErrors() {
				t.Fatal(diags.Error())
			}
			children := map[string]bool{}
			for _, block := range file.Body.(*hclsyntax.Body).Blocks {
				if block.Type != "resource" || block.Labels[0] != "spacelift_stack" {
					continue
				}
				groupName := strings.TrimPrefix(block.Labels[1], "admin_split_")
				children[groupName] = true
				for name, want := range map[string]string{
					"name": "existing-admin-" + groupName, "space_id": "root",
					"worker_pool_id": "original-private-pool", "runner_image": "registry.example.com/custom-runner:stable",
					"repository": "migration-admin", "branch": "main", "project_root": "admin/_split/" + groupName,
				} {
					attr := block.Body.Attributes[name]
					if attr == nil {
						t.Fatalf("child %s omitted %s", groupName, name)
					}
					got, diags := attr.Expr.Value(nil)
					if diags.HasErrors() || !got.RawEquals(cty.StringVal(want)) {
						t.Fatalf("child %s did not preserve %s=%q", groupName, name, want)
					}
				}
				vcsFound, versionFound := false, false
				for _, nested := range block.Body.Blocks {
					if nested.Type == "spacelift_repo" {
						vcsFound = true
					}
					if nested.Type == "opentofu" {
						versionFound = true
						value, diags := nested.Body.Attributes["version"].Expr.Value(nil)
						if diags.HasErrors() || !value.RawEquals(cty.StringVal(state.TerraformVersion)) {
							t.Fatalf("child %s changed the source state's tool version", groupName)
						}
					}
				}
				if !vcsFound || !versionFound {
					t.Fatalf("child %s omitted native VCS or OpenTofu settings", groupName)
				}
			}
			if !reflect.DeepEqual(children, map[string]bool{"organization__team_a": true, "organization__team_b": true}) {
				t.Fatalf("unexpected child admin resources: %v", children)
			}
		})
	}
}

func TestSourceStackChoicesRejectRuntimeOverrides(t *testing.T) {
	config, snapshot, _, tool, _ := migrationKitFixture(t)
	state, _, err := readState(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	settings := sourcePrepareSettings(t, state.TerraformVersion)
	settingsPath := filepath.Join(t.TempDir(), "source-stack.json")
	writeTest(t, settingsPath, string(settings))
	for _, field := range []string{"worker_pool_id", "runner_image", "repository", "branch", "project_root", "vcs_block", "vcs_provider", "source_stack_id", "space_id", "role_id", "tool_version"} {
		t.Run(field, func(t *testing.T) {
			choices := filepath.Join(t.TempDir(), "choices.json")
			if err := writeJSON(choices, map[string]any{field: "override"}); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "prepared")
			var result bytes.Buffer
			if err := Run([]string{"prepare", "--config", config, "--state", snapshot, "--source-stack", settingsPath, "--layout", choices, "--out", out, "--tool", tool, "--validate=false"}, &result); err == nil || !strings.Contains(err.Error(), "only groups and variables") {
				t.Fatalf("expected runtime override rejection, got %v", err)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("rejected override created output: %v", err)
			}
		})
	}
}

func TestSourceStackChoicesAllowGroupsAndVariables(t *testing.T) {
	config, snapshot, _, tool, _ := migrationKitFixture(t)
	state, _, err := readState(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	choices := filepath.Join(t.TempDir(), "choices.json")
	if err := writeJSON(choices, map[string]any{
		"groups":    []group{{Name: "selected_team", Module: "module.organization.module.team_b"}},
		"variables": map[string]any{"ancestor_space_ids": []string{"custom-ancestor"}},
	}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "prepared")
	report, err := prepareFromStack(config, snapshot, sourcePrepareSettings(t, state.TerraformVersion), choices, out, tool, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.ParentInstances != 17 || len(report.Groups) != 1 || report.Groups[0].Name != "selected_team" || report.Groups[0].Module != "module.organization.module.team_b" || report.Groups[0].Instances != 12 {
		t.Fatalf("explicit group selection was not applied: %+v", report)
	}
	raw, err := os.ReadFile(filepath.Join(out, "repo", "_split", "selected_team", "main.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("custom-ancestor")) {
		t.Fatal("layout variable did not reach the selected child's bootstrap inputs")
	}
}

func TestManualLayoutRequiresExplicitWorkerPool(t *testing.T) {
	config, snapshot, layoutPath, tool, _ := migrationKitFixture(t)
	raw, err := os.ReadFile(layoutPath)
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	delete(spec, "worker_pool_id")
	if err := writeJSON(layoutPath, spec); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "prepared")
	if _, err := prepare(config, snapshot, layoutPath, out, tool, false); err == nil || !strings.Contains(err.Error(), "worker pool is unspecified") {
		t.Fatalf("expected omitted worker pool rejection, got %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("omitted pool created output: %v", err)
	}
}
