package splitter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

type layout struct {
	SourceStackID string                     `json:"source_stack_id"`
	SpaceID       string                     `json:"space_id"`
	RoleID        string                     `json:"role_id"`
	WorkerPoolID  string                     `json:"worker_pool_id"`
	RunnerImage   string                     `json:"runner_image,omitempty"`
	Repository    string                     `json:"repository"`
	Branch        string                     `json:"branch"`
	ToolVersion   string                     `json:"tool_version"`
	VCSBlock      string                     `json:"vcs_block"`
	VCSProvider   string                     `json:"vcs_provider"`
	ProjectRoot   string                     `json:"project_root"`
	Variables     map[string]json.RawMessage `json:"variables"`
	Groups        []group                    `json:"groups"`
}

type group struct {
	Name      string `json:"name"`
	Module    string `json:"module"`
	StackName string `json:"stack_name,omitempty"`
}

type moduleFile struct {
	path   string
	raw    []byte
	syntax *hclsyntax.Body
	write  *hclwrite.File
}
type moduleNode struct {
	dir      string
	prefix   string
	files    []*moduleFile
	parent   *moduleNode
	call     *hclsyntax.Block
	callFile *moduleFile
	name     string
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

func moduleNames(address string) ([]string, error) {
	parts := strings.Split(address, ".")
	if len(parts) == 0 || len(parts)%2 != 0 {
		return nil, fmt.Errorf("invalid module address %q", address)
	}
	var names []string
	for i := 0; i < len(parts); i += 2 {
		if parts[i] != "module" || !identifier.MatchString(parts[i+1]) {
			return nil, fmt.Errorf("module address %q must name unindexed module calls", address)
		}
		names = append(names, parts[i+1])
	}
	return names, nil
}

func readModule(dir, prefix string) (*moduleNode, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	n := &moduleNode{dir: dir, prefix: prefix}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".tf.json") {
			return nil, fmt.Errorf("JSON configuration is unsupported: %s", e.Name())
		}
		if !strings.HasSuffix(e.Name(), ".tf") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		f, d := hclsyntax.ParseConfig(raw, path, hcl.InitialPos)
		if d.HasErrors() {
			return nil, fmt.Errorf("invalid HCL in %s", path)
		}
		w, d := hclwrite.ParseConfig(raw, path, hcl.InitialPos)
		if d.HasErrors() {
			return nil, fmt.Errorf("invalid HCL in %s", path)
		}
		n.files = append(n.files, &moduleFile{path: path, raw: raw, syntax: f.Body.(*hclsyntax.Body), write: w})
	}
	if len(n.files) == 0 {
		return nil, fmt.Errorf("no .tf configuration in %s", dir)
	}
	return n, nil
}

func locate(root string, names []string) ([]*moduleNode, error) {
	n, err := readModule(root, "")
	if err != nil {
		return nil, err
	}
	nodes := []*moduleNode{n}
	for _, name := range names {
		var call *hclsyntax.Block
		var file *moduleFile
		for _, f := range n.files {
			for _, b := range f.syntax.Blocks {
				if b.Type == "module" && len(b.Labels) == 1 && b.Labels[0] == name {
					if call != nil {
						return nil, fmt.Errorf("duplicate module %s", name)
					}
					call = b
					file = f
				}
			}
		}
		if call == nil {
			return nil, fmt.Errorf("module %s not found in %s", name, n.dir)
		}
		for _, meta := range []string{"count", "for_each", "providers"} {
			if call.Body.Attributes[meta] != nil {
				return nil, fmt.Errorf("module %s uses %s; indexed calls and explicit provider mappings require a manual split", name, meta)
			}
		}
		a := call.Body.Attributes["source"]
		if a == nil {
			return nil, fmt.Errorf("module %s has no source", name)
		}
		v, d := a.Expr.Value(nil)
		if d.HasErrors() || v.Type() != cty.String {
			return nil, fmt.Errorf("module %s requires a static local source", name)
		}
		source := v.AsString()
		if !strings.HasPrefix(source, "./") && !strings.HasPrefix(source, "../") {
			return nil, fmt.Errorf("selected module %s requires a local source", name)
		}
		dir := filepath.Clean(filepath.Join(n.dir, source))
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("module %s escapes the configuration root", name)
		}
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return nil, err
		}
		if real != dir {
			return nil, fmt.Errorf("module %s uses a symlink; copy a self-contained configuration first", name)
		}
		prefix := "module." + name
		if n.prefix != "" {
			prefix = n.prefix + "." + prefix
		}
		child, err := readModule(dir, prefix)
		if err != nil {
			return nil, err
		}
		child.parent = n
		child.call = call
		child.callFile = file
		child.name = name
		nodes = append(nodes, child)
		n = child
	}
	return nodes, nil
}

func evalFunctions() map[string]function.Function {
	return map[string]function.Function{"concat": stdlib.ConcatFunc, "merge": stdlib.MergeFunc, "jsonencode": stdlib.JSONEncodeFunc, "jsondecode": stdlib.JSONDecodeFunc, "length": stdlib.LengthFunc, "format": stdlib.FormatFunc, "sort": stdlib.SortFunc, "tolist": stdlib.MakeToFunc(cty.List(cty.DynamicPseudoType)), "tomap": stdlib.MakeToFunc(cty.Map(cty.DynamicPseudoType)), "toset": stdlib.MakeToFunc(cty.Set(cty.DynamicPseudoType)), "tostring": stdlib.MakeToFunc(cty.String)}
}

func stateContext(state *stateFile, prefix string) (map[string]cty.Value, error) {
	types := map[string]map[string]cty.Value{}
	for _, r := range state.Resources {
		if r.Module != prefix || r.Mode != "managed" {
			continue
		}
		items := map[string]cty.Value{}
		var single cty.Value
		indexed := false
		maxIndex := -1
		for _, inst := range r.Instances {
			var v ctyjson.SimpleJSONValue
			if err := v.UnmarshalJSON(inst.Attributes); err != nil {
				return nil, fmt.Errorf("cannot decode attributes for %s.%s", r.Type, r.Name)
			}
			if len(inst.Sensitive) > 0 && string(inst.Sensitive) != "[]" && string(inst.Sensitive) != "null" {
				v.Value = v.Value.Mark("sensitive")
			}
			switch key := inst.IndexKey.(type) {
			case nil:
				single = v.Value
			case string:
				indexed = true
				items[key] = v.Value
			case float64:
				indexed = true
				i := int(key)
				if float64(i) != key || i < 0 {
					return nil, fmt.Errorf("invalid numeric instance index")
				}
				if i > maxIndex {
					maxIndex = i
				}
				items[fmt.Sprint(i)] = v.Value
			default:
				return nil, fmt.Errorf("unsupported instance index")
			}
		}
		if len(r.Instances) == 0 {
			continue
		}
		var value cty.Value
		if !indexed {
			value = single
		} else if maxIndex >= 0 {
			vals := make([]cty.Value, maxIndex+1)
			for i := range vals {
				v, ok := items[fmt.Sprint(i)]
				if !ok {
					return nil, fmt.Errorf("sparse count instances require a manual split")
				}
				vals[i] = v
			}
			value = cty.TupleVal(vals)
		} else {
			value = cty.ObjectVal(items)
		}
		if types[r.Type] == nil {
			types[r.Type] = map[string]cty.Value{}
		}
		types[r.Type][r.Name] = value
	}
	ctx := map[string]cty.Value{}
	for typ, resources := range types {
		ctx[typ] = cty.ObjectVal(resources)
	}
	return ctx, nil
}

func inputsFor(nodes []*moduleNode, state *stateFile, provided map[string]json.RawMessage) (map[string]cty.Value, error) {
	vars := map[string]cty.Value{}
	for k, raw := range provided {
		var v ctyjson.SimpleJSONValue
		if err := v.UnmarshalJSON(raw); err != nil {
			return nil, fmt.Errorf("invalid root variable %s", k)
		}
		vars[k] = v.Value
	}
	for index, n := range nodes[:len(nodes)-1] {
		ctxVars, err := stateContext(state, n.prefix)
		if err != nil {
			return nil, err
		}
		functions := evalFunctions()
		for _, f := range n.files {
			for _, b := range f.syntax.Blocks {
				if b.Type == "provider" {
					if index > 0 || len(b.Body.Attributes) > 0 || len(b.Body.Blocks) > 0 {
						return nil, fmt.Errorf("configured or aliased providers on a split boundary require manual wiring")
					}
				}
				if b.Type != "variable" || len(b.Labels) != 1 {
					continue
				}
				name := b.Labels[0]
				if _, ok := vars[name]; !ok {
					a := b.Body.Attributes["default"]
					if a == nil {
						return nil, fmt.Errorf("variable %s needs a value in layout.variables", name)
					}
					v, d := a.Expr.Value(&hcl.EvalContext{Functions: functions})
					if d.HasErrors() {
						return nil, fmt.Errorf("cannot evaluate default for variable %s", name)
					}
					vars[name] = v
				}
				if a := b.Body.Attributes["sensitive"]; a != nil {
					v, d := a.Expr.Value(nil)
					if !d.HasErrors() && v.RawEquals(cty.True) {
						vars[name] = vars[name].Mark("sensitive")
					}
				}
			}
		}
		ctxVars["var"] = cty.ObjectVal(vars)
		locals := map[string]cty.Value{}
		pending := map[string]hcl.Expression{}
		for _, f := range n.files {
			for _, b := range f.syntax.Blocks {
				if b.Type == "locals" {
					for name, a := range b.Body.Attributes {
						pending[name] = a.Expr
					}
				}
			}
		}
		for len(pending) > 0 {
			progress := false
			ctxVars["local"] = cty.ObjectVal(locals)
			for name, expr := range pending {
				v, d := expr.Value(&hcl.EvalContext{Variables: ctxVars, Functions: functions})
				if !d.HasErrors() {
					locals[name] = v
					delete(pending, name)
					progress = true
				}
			}
			if !progress {
				return nil, fmt.Errorf("locals in %s need unsupported functions or cross-module values", n.dir)
			}
		}
		ctxVars["local"] = cty.ObjectVal(locals)
		next := nodes[index+1]
		args := map[string]cty.Value{}
		for name, a := range next.call.Body.Attributes {
			if name == "source" || name == "version" {
				continue
			}
			if name == "depends_on" {
				return nil, fmt.Errorf("module %s has explicit dependencies; review ordering before splitting", next.prefix)
			}
			v, d := a.Expr.Value(&hcl.EvalContext{Variables: ctxVars, Functions: functions})
			if d.HasErrors() || !v.IsWhollyKnown() {
				return nil, fmt.Errorf("cannot resolve input %s for %s from the snapshot", name, next.prefix)
			}
			_, marks := v.UnmarkDeep()
			if len(marks) > 0 {
				return nil, fmt.Errorf("input %s for %s is sensitive; refusing to inline it into child configuration", name, next.prefix)
			}
			args[name] = v
		}
		vars = args
	}
	leaf := nodes[len(nodes)-1]
	for _, f := range leaf.files {
		for _, block := range f.syntax.Blocks {
			if block.Type != "variable" || len(block.Labels) != 1 {
				continue
			}
			if attr := block.Body.Attributes["sensitive"]; attr != nil {
				value, diags := attr.Expr.Value(nil)
				if diags.HasErrors() {
					return nil, fmt.Errorf("cannot evaluate sensitivity of input %s", block.Labels[0])
				}
				if value.RawEquals(cty.True) {
					if _, passed := vars[block.Labels[0]]; passed {
						return nil, fmt.Errorf("input %s for %s is declared sensitive; refusing to inline it", block.Labels[0], leaf.prefix)
					}
				}
			}
		}
	}
	return vars, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
