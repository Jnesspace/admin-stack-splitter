package splitter

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPrepareIgnoresPrivateArtifactsInsideGitRepository(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is required to verify private artifact exclusion")
	}
	root, statePath, specPath, tool := sourceFixture(t)
	repository := filepath.Dir(root)
	gitCommand := func(args ...string) *exec.Cmd {
		cmd := exec.Command(git, append([]string{"-c", "core.excludesFile=" + os.DevNull}, args...)...)
		cmd.Dir = repository
		return cmd
	}
	if output, err := gitCommand("init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("initialize repository: %v\n%s", err, output)
	}
	out := filepath.Join(repository, "prepared")
	if _, err := prepare(root, statePath, specPath, out, tool, false); err != nil {
		t.Fatal(err)
	}
	privateFiles := 0
	err = filepath.WalkDir(filepath.Join(out, "private"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(repository, path)
		if err != nil {
			return err
		}
		if output, err := gitCommand("check-ignore", "-q", "--", relative).CombinedOutput(); err != nil {
			t.Errorf("private artifact %s is not ignored: %v\n%s", relative, err, output)
		}
		privateFiles++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if privateFiles == 0 {
		t.Fatal("preparation produced no private artifacts to check")
	}
	if err := gitCommand("check-ignore", "-q", "--", "prepared/repo/admin_split_controller.tf").Run(); err == nil {
		t.Fatal("publishable configuration was ignored along with private artifacts")
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("check publishable configuration: %v", err)
	}
}
