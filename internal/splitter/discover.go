package splitter

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

type splitPreview struct {
	SourceInstances int           `json:"source_instances"`
	ParentInstances int           `json:"parent_instances"`
	Groups          []groupResult `json:"groups"`
}

// discoverKitGroups selects populated leaf spaces, leaving shared ancestors in the parent.
func discoverKitGroups(config string, state *stateFile) ([]group, error) {
	root, err := filepath.Abs(config)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	var groups []group
	seen := map[string]bool{}
	namesUsed := map[string]bool{}
	var visit func([]string) error
	visit = func(names []string) error {
		nodes, err := locate(root, names)
		if err != nil {
			return err
		}
		n := nodes[len(nodes)-1]
		if seen[n.dir] {
			return fmt.Errorf("module directory is reused or cyclic: %s; supply explicit layout.groups", n.dir)
		}
		seen[n.dir] = true
		var children []string
		wiring := false
		for _, f := range n.files {
			if filepath.Base(f.path) == "liftoff.tf" {
				wiring = true
			}
			for _, b := range f.syntax.Blocks {
				if b.Type == "module" && len(b.Labels) == 1 {
					children = append(children, b.Labels[0])
				}
			}
		}
		if !wiring {
			return fmt.Errorf("%s has no liftoff.tf module wiring; automatic grouping requires migration-kit output, or supply explicit layout.groups", n.dir)
		}
		sort.Strings(children)
		if len(children) == 0 && len(names) > 0 && moduleInstances(state, n.prefix) > 0 {
			name := strings.Join(names, "__")
			if name == "parent" || namesUsed[name] {
				return fmt.Errorf("automatic group name %q is reserved or ambiguous; supply explicit layout.groups", name)
			}
			namesUsed[name] = true
			groups = append(groups, group{Name: name, Module: n.prefix})
		}
		for _, child := range children {
			path := append(append([]string(nil), names...), child)
			if err := visit(path); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(nil); err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("no populated leaf space modules found; flat root resources require a manual split")
	}
	return groups, nil
}

func moduleInstances(state *stateFile, prefix string) int {
	count := 0
	for _, r := range state.Resources {
		if prefix == "" || within(r.Module, prefix) {
			count += len(r.Instances)
		}
	}
	return count
}

func previewKitSplit(config string, state *stateFile) (*splitPreview, error) {
	groups, err := discoverKitGroups(config, state)
	if err != nil {
		return nil, err
	}
	result := &splitPreview{SourceInstances: moduleInstances(state, "")}
	result.ParentInstances = result.SourceInstances
	for _, g := range groups {
		count := moduleInstances(state, g.Module)
		result.Groups = append(result.Groups, groupResult{Name: g.Name, Module: g.Module, Instances: count})
		result.ParentInstances -= count
	}
	return result, nil
}
