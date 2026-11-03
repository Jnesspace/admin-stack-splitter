package splitter

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCutoverUsesActualNamesPathsAndSafeSetup(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "review user's $(touch injected) | <files>")
	report := &prepareResult{
		Complete: true, Validated: true, SourceStackID: "existing-admin", WorkerPoolID: "original-private-pool",
		ToolVersion: "1.10.6", SourceInstances: 29, ParentInstances: 5,
		Groups: []groupResult{
			{Name: "team_a", StackName: "Team A | <admin>", Instances: 12, StateFile: "private/states/team_a.tfstate", ConfigDirectory: "repo/_split/team_a"},
			{Name: "team_b", Instances: 12, StateFile: "private/states/team_b.tfstate", ConfigDirectory: "repo/_split/team_b"},
		},
	}
	for _, g := range report.Groups {
		writeTest(t, filepath.Join(dir, g.StateFile), "synthetic snapshot")
	}
	rawGuide, err := renderCutover(dir, report)
	if err != nil {
		t.Fatal(err)
	}
	guide := string(rawGuide)
	for _, expected := range []string{
		"Local preparation and configuration validation passed", "original-private-pool", "Team A &#124; &lt;admin&gt;", "existing-admin-team_b",
		"private/states/team_a.tfstate", "private/states/team_b.tfstate", "_split/team_a", "_split/team_b", "parent.tfstate", "original.tfstate",
	} {
		if !strings.Contains(guide, expected) {
			t.Errorf("cutover guide omits %q", expected)
		}
	}
	if strings.Contains(guide, "existing-admin-team_a") || strings.Contains(guide, "<admin>") || strings.Contains(guide, "STOP:") {
		t.Fatal("guide replaced a custom stack name, allowed markup injection or rejected valid preparation")
	}
	setup := strings.SplitN(strings.SplitN(guide, "```bash\n", 2)[1], "\n```", 2)[0]
	cmd := exec.Command("bash", "-c", setup+"\nprintf '%s\\n' \"$SPLIT_STACK_ID\" \"$SPLIT_PREPARED\"")
	cmd.Dir = work
	output, err := cmd.Output()
	if err != nil || string(output) != report.SourceStackID+"\n"+dir+"\n" {
		t.Fatalf("setup does not preserve shell values: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "injected")); !os.IsNotExist(err) {
		t.Fatal("generated setup executed part of an output path")
	}
	report.Validated = false
	rawGuide, err = renderCutover(dir, report)
	if err != nil {
		t.Fatal(err)
	}
	guide = string(rawGuide)
	if !strings.Contains(guide, "STOP:") || strings.Contains(guide, "validation passed") {
		t.Fatal("unvalidated preparation received a ready-to-cut-over guide")
	}
	if err := os.Truncate(filepath.Join(dir, report.Groups[0].StateFile), 2_000_001); err != nil {
		t.Fatal(err)
	}
	rawGuide, err = renderCutover(dir, report)
	if err != nil || !strings.Contains(string(rawGuide), "Delivery needed before cutover") || !strings.Contains(string(rawGuide), "2000001") {
		t.Fatalf("oversized child snapshot was not identified before cutover: %v", err)
	}
}

func TestPrepareGuidePreservesCustomChildName(t *testing.T) {
	config, snapshot, layoutPath, tool := sourceFixture(t)
	raw, err := os.ReadFile(layoutPath)
	if err != nil {
		t.Fatal(err)
	}
	var spec layout
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	spec.Groups[0].StackName = "Team A administrator"
	if err := writeJSON(layoutPath, spec); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "my split")
	report, err := prepare(config, snapshot, layoutPath, out, tool, false)
	if err != nil {
		t.Fatal(err)
	}
	guide, err := os.ReadFile(filepath.Join(out, "CUTOVER.md"))
	if err != nil {
		t.Fatal(err)
	}
	if report.Groups[0].StackName != spec.Groups[0].StackName || !strings.Contains(string(guide), spec.Groups[0].StackName) || !strings.Contains(string(guide), out) {
		t.Fatal("prepared guide does not match the configured child or chosen output directory")
	}
	if !strings.Contains(string(guide), "STOP:") {
		t.Fatal("diagnostic preparation did not stop manual cutover")
	}
}
