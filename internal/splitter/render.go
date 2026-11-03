package splitter

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

func quote(s string) string { return string(hclwrite.TokensForValue(cty.StringVal(s)).Bytes()) }

func parseWrite(source string) (*hclwrite.File, error) {
	f, d := hclwrite.ParseConfig([]byte(source), "generated.tf", hcl.InitialPos)
	if d.HasErrors() {
		return nil, fmt.Errorf("generated invalid HCL")
	}
	return f, nil
}

func preamble(n *moduleNode, providers bool) string {
	var source strings.Builder
	for _, f := range n.files {
		for _, b := range f.write.Body().Blocks() {
			if b.Type() == "terraform" || (providers && b.Type() == "provider") {
				source.Write(b.BuildTokens(nil).Bytes())
				source.WriteByte('\n')
			}
		}
	}
	file, err := parseWrite(source.String())
	if err != nil {
		return source.String()
	}
	for _, block := range file.Body().Blocks() {
		if block.Type() == "terraform" {
			for _, nested := range block.Body().Blocks() {
				if nested.Type() == "backend" || nested.Type() == "cloud" {
					block.Body().RemoveBlock(nested)
				}
			}
		}
	}
	return string(file.Bytes())
}

func renderChild(root, repo string, g preparedGroup) error {
	values, err := jsonLiteral(g.Inputs)
	if err != nil {
		return err
	}
	payload := `{"inputs":` + values + `,"revision":"bootstrap"}`
	names, err := moduleNames(g.Module)
	if err != nil {
		return err
	}
	dir := filepath.Join(repo, "_split", g.Name)
	for index, name := range names {
		source := preamble(g.Nodes[index], index == 0) + "\nvariable \"split_inputs\" {\n  type = string\n"
		if index == 0 {
			source += "  default = " + quote(payload) + "\n"
		}
		source += "}\n"
		if index == len(names)-1 {
			source += "locals { split_values = jsondecode(var.split_inputs).inputs }\n"
		}
		source += fmt.Sprintf("module %s {\n  source = %s\n", quote(name), quote("./"+name))
		if index < len(names)-1 {
			source += "  split_inputs = var.split_inputs\n"
		} else {
			for _, key := range sortedKeys(g.Inputs) {
				if !identifier.MatchString(key) {
					return fmt.Errorf("invalid module argument name")
				}
				source += fmt.Sprintf("  %s = local.split_values.%s\n", key, key)
			}
		}
		source += "}\n"
		if err := writeHCL(filepath.Join(dir, "main.tf"), source); err != nil {
			return err
		}
		dir = filepath.Join(dir, name)
	}
	if err := copyConfiguration(g.Nodes[len(g.Nodes)-1].dir, dir); err != nil {
		return err
	}
	if lock, err := os.ReadFile(filepath.Join(root, ".terraform.lock.hcl")); err == nil {
		return writePrivate(filepath.Join(repo, "_split", g.Name, ".terraform.lock.hcl"), lock)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func detachModule(repo string, g preparedGroup) error {
	names, err := moduleNames(g.Module)
	if err != nil {
		return err
	}
	nodes, err := locate(repo, names)
	if err != nil {
		return err
	}
	leaf := nodes[len(nodes)-1]
	file := leaf.callFile
	var call *hclwrite.Block
	for _, b := range file.write.Body().Blocks() {
		if b.Type() == "module" && len(b.Labels()) == 1 && b.Labels()[0] == leaf.name {
			call = b
			break
		}
	}
	if call == nil {
		return fmt.Errorf("selected module disappeared")
	}
	var attrs []hclwrite.ObjectAttrTokens
	for _, key := range sortedKeys(call.Body().Attributes()) {
		if key == "source" || key == "version" {
			continue
		}
		attrs = append(attrs, hclwrite.ObjectAttrTokens{Name: hclwrite.TokensForIdentifier(key), Value: call.Body().GetAttribute(key).Expr().BuildTokens(nil)})
	}
	expr := string(hclwrite.TokensForFunctionCall("jsonencode", hclwrite.TokensForObject(attrs)).Bytes())
	file.write.Body().RemoveBlock(call)
	if err := writePrivate(file.path, file.write.Bytes()); err != nil {
		return err
	}
	output := "split_" + g.Name + "_inputs"
	for index := len(nodes) - 2; index >= 0; index-- {
		n := nodes[index]
		if index == 0 {
			digest := fmt.Sprintf(`sha256(join("", [for p in sort(fileset("${path.module}/_split/%s", "**")) : filesha256("${path.module}/_split/%s/${p}") if !strcontains("/${p}", "/.terraform/") && (endswith(p, ".tf") || endswith(p, ".terraform.lock.hcl"))]))`, g.Name, g.Name)
			expr = "jsonencode({ inputs = jsondecode(" + expr + "), revision = " + digest + " })"
			output = "split_" + g.Name + "_payload"
		}
		path := filepath.Join(n.dir, "admin_split_outputs.tf")
		f := hclwrite.NewEmptyFile()
		if raw, err := os.ReadFile(path); err == nil {
			f, d := hclwrite.ParseConfig(raw, path, hcl.InitialPos)
			if d.HasErrors() {
				return fmt.Errorf("invalid generated output file")
			}
			if hasOutput(n, output) {
				return fmt.Errorf("output name collision: %s", output)
			}
			b := hclwrite.NewBlock("output", []string{output})
			b.Body().SetAttributeRaw("value", expressionTokens(expr))
			f.Body().AppendBlock(b)
			if err := writePrivate(path, f.Bytes()); err != nil {
				return err
			}
		} else if os.IsNotExist(err) {
			if hasOutput(n, output) {
				return fmt.Errorf("output name collision: %s", output)
			}
			b := hclwrite.NewBlock("output", []string{output})
			b.Body().SetAttributeRaw("value", expressionTokens(expr))
			f.Body().AppendBlock(b)
			if err := writePrivate(path, f.Bytes()); err != nil {
				return err
			}
		} else {
			return err
		}
		if index > 0 {
			expr = "module." + n.name + "." + output
		}
	}
	return nil
}

func expressionTokens(expr string) hclwrite.Tokens {
	f, err := parseWrite("x = " + expr + "\n")
	if err != nil {
		panic("internally generated expression is invalid")
	}
	return f.Body().GetAttribute("x").Expr().BuildTokens(nil)
}
func hasOutput(n *moduleNode, name string) bool {
	for _, f := range n.files {
		for _, b := range f.syntax.Blocks {
			if b.Type == "output" && len(b.Labels) == 1 && b.Labels[0] == name {
				return true
			}
		}
	}
	return false
}

func renderController(repo string, spec layout, groups []preparedGroup) ([]string, error) {
	vcs := spec.VCSBlock
	if vcs == "" && (spec.VCSProvider == "" || spec.VCSProvider == "SPACELIFT") {
		vcs = "spacelift_repo {}"
	}
	if vcs == "" && spec.VCSProvider != "GITHUB" {
		return nil, fmt.Errorf("vcs_block is required for this VCS provider")
	}
	projectRoot := path.Clean(spec.ProjectRoot)
	if path.IsAbs(projectRoot) || projectRoot == ".." || strings.HasPrefix(projectRoot, "../") {
		return nil, fmt.Errorf("project_root must be a relative repository path")
	}
	f, d := hclsyntax.ParseConfig([]byte(vcs), "vcs", hcl.InitialPos)
	if d.HasErrors() {
		return nil, fmt.Errorf("invalid vcs_block")
	}
	body := f.Body.(*hclsyntax.Body)
	allowed := map[string]bool{"spacelift_repo": true, "github_enterprise": true, "gitlab": true, "azure_devops": true, "bitbucket_cloud": true, "bitbucket_datacenter": true, "raw_git": true}
	if len(body.Attributes) != 0 || len(body.Blocks) > 1 || (len(body.Blocks) == 1 && !allowed[body.Blocks[0].Type]) {
		return nil, fmt.Errorf("vcs_block must be one VCS configuration block")
	}
	var source strings.Builder
	workerPool := ""
	if spec.WorkerPoolID != "" {
		workerPool = "worker_pool_id = " + quote(spec.WorkerPoolID)
	}
	if spec.RunnerImage != "" {
		workerPool += "\nrunner_image = " + quote(spec.RunnerImage)
	}
	creates := []string{"spacelift_policy.admin_split_push"}
	fmt.Fprintf(&source, `resource "spacelift_policy" "admin_split_push" {
  name = %s
  space_id = %s
  type = "GIT_PUSH"
  engine_type = "REGO_V1"
  body = "package spacelift\ntrack if input.push.branch == input.stack.branch\nnotrigger := true\n"
}
`, quote(spec.SourceStackID+"-child-push"), quote(spec.SpaceID))
	for _, g := range groups {
		key := "admin_split_" + g.Name
		name := g.StackName
		if name == "" {
			name = spec.SourceStackID + "-" + g.Name
		}
		fmt.Fprintf(&source, `resource "spacelift_stack" %s {
  name = %s
  space_id = %s
  repository = %s
  branch = %s
  project_root = %s
  %s
  %s
  manage_state = true
  autodeploy = false
  labels = ["admin-split-child"]
  import_state_file = %s
  lifecycle { ignore_changes = [import_state_file] }
  opentofu { version = %s }
}
resource "spacelift_role_attachment" %s {
  role_id = %s
  space_id = %s
  stack_id = spacelift_stack.%s.id
}
resource "spacelift_policy_attachment" %s {
  policy_id = spacelift_policy.admin_split_push.id
  stack_id = spacelift_stack.%s.id
}
resource "spacelift_stack_dependency" %s {
  stack_id = spacelift_stack.%s.id
  depends_on_stack_id = %s
}
resource "spacelift_stack_dependency_reference" %s {
  stack_dependency_id = spacelift_stack_dependency.%s.id
  output_name = %s
  input_name = "TF_VAR_split_inputs"
}
`, quote(key), quote(name), quote(spec.SpaceID), quote(spec.Repository), quote(spec.Branch), quote(path.Join(spec.ProjectRoot, "_split", g.Name)), vcs, workerPool, quote("/mnt/workspace/"+g.Name+".tfstate"), quote(spec.ToolVersion), quote(key), quote(spec.RoleID), quote(spec.SpaceID), key, quote(key), key, quote(key), key, quote(spec.SourceStackID), quote(key), key, quote("split_"+g.Name+"_payload"))
		for _, typ := range []string{"spacelift_stack", "spacelift_role_attachment", "spacelift_policy_attachment", "spacelift_stack_dependency", "spacelift_stack_dependency_reference"} {
			creates = append(creates, typ+"."+key)
		}
	}
	n, err := readModule(repo, "")
	if err != nil {
		return nil, err
	}
	for _, file := range n.files {
		for _, b := range file.syntax.Blocks {
			if b.Type == "resource" && len(b.Labels) == 2 {
				address := b.Labels[0] + "." + b.Labels[1]
				for _, create := range creates {
					if address == create {
						return nil, fmt.Errorf("controller resource address already exists: %s", address)
					}
				}
			}
		}
	}
	if err := writeHCL(filepath.Join(repo, "admin_split_controller.tf"), source.String()); err != nil {
		return nil, err
	}
	return creates, nil
}
