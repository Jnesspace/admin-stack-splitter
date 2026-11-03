package splitter

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/zclconf/go-cty/cty"
)

func checkModuleClosure(selected string) error {
	root, err := filepath.Abs(selected)
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	// Check copied paths before parsing dependency files, without following links.
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != root && entry.IsDir() && strings.HasPrefix(entry.Name(), ".") {
			return filepath.SkipDir
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			rel, _ := filepath.Rel(root, path)
			return fmt.Errorf("selected module contains symlink %s; copy a self-contained configuration first", rel)
		}
		return nil
	}); err != nil {
		return err
	}
	active, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(dir string) error {
		if active[dir] {
			return fmt.Errorf("local module cycle within selected subtree")
		}
		if visited[dir] {
			return nil
		}
		active[dir] = true
		defer delete(active, dir)
		node, err := readModule(dir, "")
		if err != nil {
			return err
		}
		for _, file := range node.files {
			for _, block := range file.syntax.Blocks {
				if block.Type != "module" {
					continue
				}
				if len(block.Labels) != 1 {
					return fmt.Errorf("invalid module call within selected subtree")
				}
				name := block.Labels[0]
				attr := block.Body.Attributes["source"]
				if attr == nil {
					return fmt.Errorf("module %s within selected subtree has no source", name)
				}
				value, diags := attr.Expr.Value(nil)
				if diags.HasErrors() || !value.IsKnown() || value.IsNull() || value.Type() != cty.String {
					return fmt.Errorf("module %s within selected subtree requires a static source", name)
				}
				source := value.AsString()
				if filepath.IsAbs(source) || strings.HasPrefix(source, "file://") {
					return fmt.Errorf("module %s uses an absolute or file URL source; use relative modules inside the selected subtree", name)
				}
				if source != "." && source != ".." && !strings.HasPrefix(source, "./") && !strings.HasPrefix(source, "../") {
					continue
				}
				target := filepath.Clean(filepath.Join(dir, source))
				rel, err := filepath.Rel(root, target)
				if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return fmt.Errorf("module %s depends on a local directory outside the selected module subtree; select a common ancestor or make the subtree self-contained", name)
				}
				if rel != "." {
					for _, part := range strings.Split(rel, string(filepath.Separator)) {
						if strings.HasPrefix(part, ".") {
							return fmt.Errorf("module %s depends on a hidden directory that is not copied", name)
						}
					}
				}
				real, err := filepath.EvalSymlinks(target)
				if err != nil {
					return fmt.Errorf("resolve local module %s: %w", name, err)
				}
				if real != target {
					return fmt.Errorf("module %s uses a symlink; copy a self-contained configuration first", name)
				}
				if err := visit(target); err != nil {
					return err
				}
			}
		}
		visited[dir] = true
		return nil
	}
	return visit(root)
}
