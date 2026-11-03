package splitter

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareAutomaticMigrationKitGroups(t *testing.T) {
	config, snapshot, layoutPath, tool, _ := migrationKitFixture(t)
	raw, err := os.ReadFile(layoutPath)
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	delete(spec, "groups")
	if err := writeJSON(layoutPath, spec); err != nil {
		t.Fatal(err)
	}
	var inspected bytes.Buffer
	if err := Run([]string{"inspect", "--config", config, "--state", snapshot}, &inspected); err != nil {
		t.Fatal(err)
	}
	var preview splitPreview
	if err := json.Unmarshal(inspected.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	wantGroups := []groupResult{
		{Name: "organization__team_a", Module: "module.organization.module.team_a", Instances: 12},
		{Name: "organization__team_b", Module: "module.organization.module.team_b", Instances: 12},
	}
	if preview.SourceInstances != 29 || preview.ParentInstances != 5 || !reflect.DeepEqual(preview.Groups, wantGroups) {
		t.Fatalf("unexpected automatic preview: %+v", preview)
	}
	out := filepath.Join(t.TempDir(), "prepared")
	var prepared bytes.Buffer
	if err := Run([]string{"prepare", "--config", config, "--state", snapshot, "--layout", layoutPath, "--out", out, "--tool", tool, "--validate=false"}, &prepared); err != nil {
		t.Fatal(err)
	}
	var report prepareResult
	if err := json.Unmarshal(prepared.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.Validated || report.SourceInstances != preview.SourceInstances || report.ParentInstances != preview.ParentInstances || len(report.Groups) != len(wantGroups) {
		t.Fatalf("preparation differs from inspection: %+v", report)
	}
	for index, group := range report.Groups {
		want := wantGroups[index]
		want.StateFile = "private/states/" + want.Name + ".tfstate"
		want.ConfigDirectory = "repo/_split/" + want.Name
		if group != want {
			t.Fatalf("unexpected prepared group: %+v, want %+v", group, want)
		}
		state, _, err := readState(filepath.Join(out, filepath.FromSlash(group.StateFile)))
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Resources) != 12 {
			t.Fatalf("%s contains %d resources, want 12", group.Name, len(state.Resources))
		}
		for _, resource := range state.Resources {
			if resource.Module != group.Module {
				t.Fatalf("%s owns unexpected module %s", group.Name, resource.Module)
			}
		}
	}
}

func TestPrepareRejectsInvalidGroupNamesBeforeOutput(t *testing.T) {
	for _, test := range []struct {
		name   string
		groups []group
		want   string
	}{
		{name: "reserved parent", groups: []group{{Name: "parent", Module: "module.organization.module.team_a"}}, want: "reserved"},
		{name: "duplicate names", groups: []group{{Name: "team", Module: "module.organization.module.team_a"}, {Name: "team", Module: "module.organization.module.team_b"}}, want: "unique"},
		{name: "duplicate modules", groups: []group{{Name: "first", Module: "module.organization.module.team_a"}, {Name: "second", Module: "module.organization.module.team_a"}}, want: "selected twice"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, snapshot, layoutPath, tool, _ := migrationKitFixture(t)
			raw, err := os.ReadFile(layoutPath)
			if err != nil {
				t.Fatal(err)
			}
			var spec layout
			if err := json.Unmarshal(raw, &spec); err != nil {
				t.Fatal(err)
			}
			spec.Groups = test.groups
			if err := writeJSON(layoutPath, spec); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "prepared")
			if _, err := prepare(config, snapshot, layoutPath, out, tool, false); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q error, got %v", test.want, err)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("invalid grouping created output: %v", err)
			}
		})
	}
}

func TestAutomaticGroupsIgnoreUnpopulatedLeaf(t *testing.T) {
	config, snapshot, _, _, _ := migrationKitFixture(t)
	state, _, err := readState(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var retained []stateResource
	for _, resource := range state.Resources {
		if resource.Module != "module.organization.module.team_b" {
			retained = append(retained, resource)
		}
	}
	state.Resources = retained
	preview, err := previewKitSplit(config, state)
	if err != nil {
		t.Fatal(err)
	}
	if preview.SourceInstances != 17 || preview.ParentInstances != 5 || len(preview.Groups) != 1 || preview.Groups[0].Name != "organization__team_a" || preview.Groups[0].Instances != 12 {
		t.Fatalf("unexpected grouping with empty leaf: %+v", preview)
	}
}

func TestAutomaticGroupsRequireKitWiring(t *testing.T) {
	config, snapshot, _, _, _ := migrationKitFixture(t)
	if err := os.Remove(filepath.Join(config, "liftoff.tf")); err != nil {
		t.Fatal(err)
	}
	state, _, err := readState(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := discoverKitGroups(config, state); err == nil || !strings.Contains(err.Error(), "liftoff.tf") {
		t.Fatalf("expected missing kit wiring error, got %v", err)
	}
}

func TestPrepareCommandHelp(t *testing.T) {
	var out bytes.Buffer
	if err := Run([]string{"prepare", "--help"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, option := range []string{"-config", "-state", "-layout", "-out", "-tool"} {
		if !strings.Contains(out.String(), option) {
			t.Fatalf("prepare help omits %s: %s", option, out.String())
		}
	}
}
