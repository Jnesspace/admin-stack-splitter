package splitter

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModuleClosure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{name: "descendants", files: map[string]string{
			"main.tf":             `module "team" { source = "./team" }`,
			"team/main.tf":        `module "nested" { source = "./nested" }`,
			"team/nested/main.tf": `locals { ready = true }`,
		}},
		{name: "sibling within selected subtree", files: map[string]string{
			"main.tf":        `module "team" { source = "./team" }`,
			"team/main.tf":   `module "shared" { source = "../shared" }`,
			"shared/main.tf": `locals { ready = true }`,
		}},
		{name: "reused descendant", files: map[string]string{
			"main.tf":        "module \"a\" { source = \"./shared\" }\nmodule \"b\" { source = \"./shared\" }\n",
			"shared/main.tf": `locals { ready = true }`,
		}},
		{name: "remote source", files: map[string]string{"main.tf": `module "remote" { source = "example/module/provider" }`}},
		{name: "sibling outside selected subtree", files: map[string]string{"main.tf": `module "shared" { source = "../shared" }`}, want: "outside the selected module subtree"},
		{name: "nested escape", files: map[string]string{
			"main.tf":      `module "team" { source = "./team" }`,
			"team/main.tf": `module "shared" { source = "../../shared" }`,
		}, want: "outside the selected module subtree"},
		{name: "cycle", files: map[string]string{
			"main.tf":      `module "team" { source = "./team" }`,
			"team/main.tf": `module "parent" { source = "../" }`,
		}, want: "cycle"},
		{name: "hidden dependency", files: map[string]string{
			"main.tf":         `module "hidden" { source = "./.hidden" }`,
			".hidden/main.tf": `locals { ready = true }`,
		}, want: "hidden directory"},
		{name: "file URL", files: map[string]string{"main.tf": `module "external" { source = "file:///outside" }`}, want: "file URL"},
		{name: "dynamic source", files: map[string]string{"main.tf": `module "dynamic" { source = var.path }`}, want: "static source"},
		{name: "null source", files: map[string]string{"main.tf": `module "invalid" { source = null }`}, want: "static source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for path, contents := range tc.files {
				writeTest(t, filepath.Join(root, path), contents)
			}
			err := checkModuleClosure(root)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestModuleClosureRejectsAbsoluteSources(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTest(t, filepath.Join(root, "main.tf"), "module \"external\" { source = "+quote(outside)+" }\n")
	if err := checkModuleClosure(root); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("accepted an absolute module source: %v", err)
	}
}

func TestModuleClosureRejectsSymlinksBeforeParsing(t *testing.T) {
	for _, filename := range []string{"linked.tf", "nested"} {
		t.Run(filename, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			writeTest(t, filepath.Join(root, "main.tf"), `module "nested" { source = "./nested" }`)
			writeTest(t, filepath.Join(outside, "secret"), "private contents that are not HCL")
			target := outside
			if strings.HasSuffix(filename, ".tf") {
				target = filepath.Join(outside, "secret")
			}
			if err := os.Symlink(target, filepath.Join(root, filename)); err != nil {
				t.Fatal(err)
			}
			if err := checkModuleClosure(root); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("got %v, want symlink refusal before parsing", err)
			}
		})
	}
}

func TestPrepareRejectsEscapingModuleBeforeWriting(t *testing.T) {
	root, statePath, specPath, tool := sourceFixture(t)
	path := filepath.Join(root, "org-source", "team-a-source", "main.tf")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, path, string(raw)+"\nmodule \"sibling\" { source = \"../team-b-source\" }\n")
	original, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "prepared")
	if _, err := prepare(root, statePath, specPath, out, tool, false); err == nil || !strings.Contains(err.Error(), "outside the selected module subtree") {
		t.Fatalf("got %v, want refusal before state movement", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("created output before rejecting an external module dependency: %v", err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("source state changed")
	}
}
