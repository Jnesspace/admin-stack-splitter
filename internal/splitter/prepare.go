package splitter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

type preparedGroup struct {
	group
	Nodes  []*moduleNode
	Inputs map[string]cty.Value
	Count  int
}
type prepareResult struct {
	Complete          bool          `json:"complete"`
	Validated         bool          `json:"validated"`
	SourceStackID     string        `json:"source_stack_id"`
	WorkerPoolID      string        `json:"worker_pool_id"`
	RunnerImage       string        `json:"runner_image,omitempty"`
	SourceSHA256      string        `json:"source_sha256"`
	SourceLineage     string        `json:"source_lineage"`
	SourceSerial      uint64        `json:"source_serial"`
	ToolVersion       string        `json:"tool_version"`
	SourceInstances   int           `json:"source_instances"`
	ParentInstances   int           `json:"parent_instances"`
	Groups            []groupResult `json:"groups"`
	ControllerCreates []string      `json:"controller_creates"`
}
type groupResult struct {
	Name            string `json:"name"`
	StackName       string `json:"stack_name,omitempty"`
	Module          string `json:"module"`
	Instances       int    `json:"instances"`
	StateFile       string `json:"state_file"`
	ConfigDirectory string `json:"config_directory"`
}

func prepare(config, statePath, layoutPath, out, tool string, validate bool) (*prepareResult, error) {
	root, err := filepath.Abs(config)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	root = real
	for _, name := range []string{"_split", "admin_split_outputs.tf", "admin_split_controller.tf"} {
		if _, err := os.Lstat(filepath.Join(root, name)); err == nil {
			return nil, fmt.Errorf("source already contains reserved split path %s", name)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	dest, err := filepath.Abs(out)
	if err != nil {
		return nil, err
	}
	parentDir, err := filepath.EvalSymlinks(filepath.Dir(dest))
	if err != nil {
		return nil, err
	}
	dest = filepath.Join(parentDir, filepath.Base(dest))
	rel, err := filepath.Rel(root, dest)
	if err != nil || rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..") {
		return nil, fmt.Errorf("output must be outside the source configuration")
	}
	raw, err := os.ReadFile(layoutPath)
	if err != nil {
		return nil, err
	}
	var spec layout
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("invalid layout JSON: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("invalid layout JSON")
	}
	if _, present := fields["worker_pool_id"]; !present {
		return nil, fmt.Errorf("worker pool is unspecified: use --stack or --source-stack to inherit it; manual layouts must explicitly set worker_pool_id (empty only for a public source)")
	}
	if spec.SourceStackID == "" || spec.SpaceID == "" || spec.RoleID == "" || spec.Repository == "" {
		return nil, fmt.Errorf("layout requires source_stack_id, space_id, role_id and repository")
	}
	if spec.Branch == "" {
		spec.Branch = "main"
	}
	state, original, err := readState(statePath)
	if err != nil {
		return nil, err
	}
	if len(spec.Groups) == 0 {
		spec.Groups, err = discoverKitGroups(root, state)
		if err != nil {
			return nil, err
		}
	}
	if spec.ToolVersion == "" {
		spec.ToolVersion = state.TerraformVersion
	}
	if spec.ToolVersion != state.TerraformVersion {
		return nil, fmt.Errorf("layout tool_version must equal the source snapshot version %s", state.TerraformVersion)
	}
	resolved, err := exec.LookPath(tool)
	if err != nil {
		return nil, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	brand, err := exec.Command(resolved, "version").Output()
	if err != nil || !bytes.HasPrefix(brand, []byte("OpenTofu v")) {
		return nil, fmt.Errorf("this version requires an OpenTofu admin state and the matching OpenTofu binary")
	}
	version, err := exec.Command(resolved, "version", "-json").Output()
	if err != nil {
		return nil, fmt.Errorf("cannot read IaC tool version")
	}
	var toolVersion struct {
		Version string `json:"terraform_version"`
	}
	if json.Unmarshal(version, &toolVersion) != nil || toolVersion.Version != state.TerraformVersion {
		return nil, fmt.Errorf("use the source IaC tool version %s; the selected binary differs", state.TerraformVersion)
	}
	groups := make([]preparedGroup, 0, len(spec.Groups))
	names := map[string]bool{}
	parents := map[string]string{}
	for _, g := range spec.Groups {
		if !identifier.MatchString(g.Name) || names[g.Name] || g.Name == "parent" {
			return nil, fmt.Errorf("group names must be unique HCL identifiers other than reserved name parent")
		}
		names[g.Name] = true
		parts, err := moduleNames(g.Module)
		if err != nil {
			return nil, err
		}
		for _, other := range spec.Groups {
			if other.Module != g.Module && within(g.Module, other.Module) {
				return nil, fmt.Errorf("selected module subtrees overlap")
			}
		}
		for _, other := range groups {
			if other.Module == g.Module {
				return nil, fmt.Errorf("module %s was selected twice", g.Module)
			}
		}
		nodes, err := locate(root, parts)
		if err != nil {
			return nil, err
		}
		if err := checkModuleClosure(nodes[len(nodes)-1].dir); err != nil {
			return nil, err
		}
		for _, n := range nodes[1 : len(nodes)-1] {
			if p, ok := parents[n.dir]; ok && p != n.prefix {
				return nil, fmt.Errorf("shared ancestor module directory requires a manual split")
			}
			parents[n.dir] = n.prefix
		}
		inputs, err := inputsFor(nodes, state, spec.Variables)
		if err != nil {
			return nil, err
		}
		count := 0
		for _, r := range state.Resources {
			if within(r.Module, g.Module) {
				count += len(r.Instances)
			}
		}
		if count == 0 {
			return nil, fmt.Errorf("module %s has no resource instances in the snapshot", g.Module)
		}
		groups = append(groups, preparedGroup{group: g, Nodes: nodes, Inputs: inputs, Count: count})
	}
	if err := checkSharedAncestors(root, parents); err != nil {
		return nil, err
	}
	if err := newDirectory(dest); err != nil {
		return nil, err
	}
	if err := writePrivate(filepath.Join(dest, ".gitignore"), []byte("/private/\n")); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(original)
	report := &prepareResult{SourceStackID: spec.SourceStackID, WorkerPoolID: spec.WorkerPoolID, RunnerImage: spec.RunnerImage, SourceSHA256: hex.EncodeToString(sum[:]), SourceLineage: state.Lineage, SourceSerial: state.Serial, ToolVersion: state.TerraformVersion}
	for _, r := range state.Resources {
		report.SourceInstances += len(r.Instances)
	}
	status := filepath.Join(dest, "prepare.json")
	if err := writeJSON(status, report); err != nil {
		return nil, err
	}
	private := filepath.Join(dest, "private")
	if err := writePrivate(filepath.Join(private, "original.tfstate"), original); err != nil {
		return nil, err
	}
	statesDir := filepath.Join(private, "states")
	if err := writePrivate(filepath.Join(statesDir, "parent.tfstate"), original); err != nil {
		return nil, err
	}
	repo := filepath.Join(dest, "repo")
	if err := copyConfiguration(root, repo); err != nil {
		return nil, err
	}
	for _, g := range groups {
		file := g.Name + ".tfstate"
		if err := localTool(resolved, statesDir, filepath.Join(private, g.Name+"-move.log"), "state", "mv", "-state=parent.tfstate", "-state-out="+file, g.Module, g.Module); err != nil {
			return nil, err
		}
		child, _, err := readState(filepath.Join(statesDir, file))
		if err != nil {
			return nil, err
		}
		if err := verifyPartition(state, child, g.Module); err != nil {
			return nil, err
		}
		if err := renderChild(root, repo, g); err != nil {
			return nil, err
		}
		if err := detachModule(repo, g); err != nil {
			return nil, err
		}
		report.Groups = append(report.Groups, groupResult{Name: g.Name, StackName: g.StackName, Module: g.Module, Instances: g.Count, StateFile: "private/states/" + file, ConfigDirectory: "repo/_split/" + g.Name})
	}
	parent, _, err := readState(filepath.Join(statesDir, "parent.tfstate"))
	if err != nil {
		return nil, err
	}
	if err := verifyRemainder(state, parent, groups); err != nil {
		return nil, err
	}
	for _, r := range parent.Resources {
		report.ParentInstances += len(r.Instances)
	}
	creates, err := renderController(repo, spec, groups)
	if err != nil {
		return nil, err
	}
	report.ControllerCreates = creates
	if err := writePrivate(filepath.Join(repo, ".gitignore"), []byte(".terraform/\n*.tfstate\n*.tfstate.*\n*.tfplan\nprivate/\n")); err != nil {
		return nil, err
	}
	if validate {
		dirs := []string{repo}
		for _, g := range groups {
			dirs = append(dirs, filepath.Join(repo, "_split", g.Name))
		}
		for i, dir := range dirs {
			log := filepath.Join(private, fmt.Sprintf("validate-%02d.log", i))
			if err := localTool(resolved, dir, log, "init", "-backend=false", "-input=false", "-no-color"); err != nil {
				return nil, err
			}
			if err := localTool(resolved, dir, log, "validate", "-no-color"); err != nil {
				return nil, err
			}
		}
		report.Validated = true
	}
	if err := filepath.WalkDir(private, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := fs.FileMode(0600)
		if d.IsDir() {
			mode = 0700
		}
		return os.Chmod(path, mode)
	}); err != nil {
		return nil, err
	}
	report.Complete = true
	guide, err := renderCutover(dest, report)
	if err != nil {
		return nil, err
	}
	if err := writePrivate(filepath.Join(dest, "CUTOVER.md"), guide); err != nil {
		return nil, err
	}
	if err := writeJSON(status, report); err != nil {
		return nil, err
	}
	return report, nil
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, append(raw, '\n'))
}

func localTool(tool, dir, log string, args ...string) error {
	f, err := os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command(tool, args...)
	cmd.Dir = dir
	cmd.Stdout = f
	cmd.Stderr = f
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if !strings.HasPrefix(key, "TF_CLI_ARGS") && key != "TF_DATA_DIR" && key != "TF_WORKSPACE" {
			cmd.Env = append(cmd.Env, env)
		}
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s failed; inspect private log %s", filepath.Base(tool), args[0], log)
	}
	return nil
}

func copyConfiguration(source, dest string) error {
	return filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dest, rel), 0700)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in configuration: %s", rel)
		}
		if !strings.HasSuffix(d.Name(), ".tf") && d.Name() != ".terraform.lock.hcl" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(d.Name(), ".tf") {
			file, diags := hclwrite.ParseConfig(raw, path, hcl.InitialPos)
			if diags.HasErrors() {
				return fmt.Errorf("invalid HCL in %s", rel)
			}
			for _, b := range file.Body().Blocks() {
				if b.Type() == "terraform" {
					for _, nested := range b.Body().Blocks() {
						if nested.Type() == "backend" || nested.Type() == "cloud" {
							b.Body().RemoveBlock(nested)
						}
					}
				}
			}
			raw = file.Bytes()
		}
		return writePrivate(filepath.Join(dest, rel), raw)
	})
}

func snapshotIdentities(state *stateFile, include func(string) bool) (map[string]string, error) {
	identities := map[string]string{}
	for _, r := range state.Resources {
		if !include(r.Module) {
			continue
		}
		for _, inst := range r.Instances {
			key := fmt.Sprintf("%s|%s|%s|%s|%v|%s", r.Module, r.Mode, r.Type, r.Name, inst.IndexKey, inst.Deposed)
			if _, ok := identities[key]; ok {
				return nil, fmt.Errorf("duplicate resource instance in state")
			}
			var value any
			decoder := json.NewDecoder(bytes.NewReader(inst.Attributes))
			decoder.UseNumber()
			if err := decoder.Decode(&value); err != nil {
				return nil, fmt.Errorf("invalid resource attributes")
			}
			canonical, err := json.Marshal(value)
			if err != nil {
				return nil, fmt.Errorf("cannot normalize resource attributes")
			}
			hash := sha256.Sum256(canonical)
			identities[key] = hex.EncodeToString(hash[:])
		}
	}
	return identities, nil
}

func compareIdentities(a, b map[string]string) error {
	if len(a) != len(b) {
		return fmt.Errorf("partition instance count changed")
	}
	for key, value := range a {
		if b[key] != value {
			return fmt.Errorf("partition changed a resource address or attribute")
		}
	}
	return nil
}
func verifyPartition(original, part *stateFile, prefix string) error {
	a, err := snapshotIdentities(original, func(m string) bool { return within(m, prefix) })
	if err != nil {
		return err
	}
	b, err := snapshotIdentities(part, func(string) bool { return true })
	if err != nil {
		return err
	}
	if part.Lineage == original.Lineage {
		return fmt.Errorf("child state must have its own lineage")
	}
	return compareIdentities(a, b)
}
func verifyRemainder(original, part *stateFile, groups []preparedGroup) error {
	a, err := snapshotIdentities(original, func(m string) bool {
		for _, g := range groups {
			if within(m, g.Module) {
				return false
			}
		}
		return true
	})
	if err != nil {
		return err
	}
	b, err := snapshotIdentities(part, func(string) bool { return true })
	if err != nil {
		return err
	}
	if part.Lineage != original.Lineage {
		return fmt.Errorf("parent state lineage changed")
	}
	return compareIdentities(a, b)
}

func checkSharedAncestors(root string, ancestors map[string]string) error {
	counts := map[string]int{}
	var visit func(string, map[string]bool) error
	visit = func(dir string, active map[string]bool) error {
		if active[dir] {
			return fmt.Errorf("local module cycle")
		}
		active[dir] = true
		defer delete(active, dir)
		n, err := readModule(dir, "")
		if err != nil {
			return err
		}
		for _, f := range n.files {
			for _, b := range f.syntax.Blocks {
				if b.Type != "module" {
					continue
				}
				a := b.Body.Attributes["source"]
				if a == nil {
					continue
				}
				v, d := a.Expr.Value(nil)
				if d.HasErrors() || v.Type() != cty.String {
					if len(ancestors) > 0 {
						return fmt.Errorf("dynamic module source prevents checking shared ancestors")
					}
					continue
				}
				s := v.AsString()
				if !strings.HasPrefix(s, "./") && !strings.HasPrefix(s, "../") {
					continue
				}
				target := filepath.Clean(filepath.Join(dir, s))
				rel, err := filepath.Rel(root, target)
				if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return fmt.Errorf("local module source escapes configuration root")
				}
				counts[target]++
				if err := visit(target, active); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(root, map[string]bool{}); err != nil {
		return err
	}
	for dir := range ancestors {
		if counts[dir] > 1 {
			return fmt.Errorf("ancestor module source is used by multiple calls; split its callers together or copy their source first")
		}
	}
	return nil
}

func jsonLiteral(values map[string]cty.Value) (string, error) {
	v := cty.ObjectVal(values)
	raw, err := ctyjson.Marshal(v, v.Type())
	if err != nil {
		return "", fmt.Errorf("cannot encode boundary inputs")
	}
	return string(raw), nil
}
