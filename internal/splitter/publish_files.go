package splitter

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

const maxPublishConfigBytes = 10 << 20

func checksum(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Read only regular files beneath the private workspace; never follow symlinks.
func readArtifact(root, name string, limit int64) ([]byte, error) {
	if !filepath.IsLocal(name) || filepath.ToSlash(filepath.Clean(name)) != name {
		return nil, fmt.Errorf("invalid prepared artifact path")
	}
	current := root
	for _, part := range strings.Split(name, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("prepared artifact is missing or uses a symlink: %s", name)
		}
	}
	f, err := os.Open(current)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("prepared artifact is not a regular file within the size limit: %s", name)
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, fmt.Errorf("cannot read prepared artifact: %s", name)
	}
	return raw, nil
}

func configFiles(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	total := 0
	err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if rel != "." && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("configuration contains a symlink")
		}
		if !strings.HasSuffix(entry.Name(), ".tf") && entry.Name() != ".terraform.lock.hcl" && entry.Name() != ".gitignore" {
			return nil
		}
		raw, err := readArtifact(root, filepath.ToSlash(rel), maxPublishConfigBytes)
		if err != nil {
			return err
		}
		total += len(raw)
		if total > maxPublishConfigBytes || len(files) >= 10000 {
			return fmt.Errorf("configuration exceeds the publication size limit")
		}
		files[filepath.ToSlash(rel)] = raw
		return nil
	})
	return files, err
}

func preparedArtifacts(root string, report *prepareResult) (map[string]string, error) {
	files, err := configFiles(filepath.Join(root, "repo"))
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for name, raw := range files {
		result["repo/"+name] = checksum(raw)
	}
	names := []string{"private/layout.json", "private/original.tfstate", "private/states/parent.tfstate"}
	for _, g := range report.Groups {
		if !identifier.MatchString(g.Name) || g.Name == "parent" || g.StateFile != "private/states/"+g.Name+".tfstate" || g.ConfigDirectory != "repo/_split/"+g.Name {
			return nil, fmt.Errorf("invalid prepared group paths")
		}
		names = append(names, g.StateFile)
	}
	for _, name := range names {
		raw, err := readArtifact(root, name, maxStateDownloadBytes)
		if err != nil {
			return nil, err
		}
		result[name] = checksum(raw)
	}
	return result, nil
}

// Freeze the checked configuration before validation and account writes. Both
// publication copies must come from these bytes, even if the source is edited.
func snapshotPublicationConfig(root, snapshot string, report *prepareResult) error {
	files, err := configFiles(filepath.Join(root, "repo"))
	if err != nil {
		return err
	}
	actual := map[string]string{}
	expected := map[string]string{}
	for name, raw := range files {
		actual["repo/"+name] = checksum(raw)
	}
	for name, sum := range report.Artifacts {
		if strings.HasPrefix(name, "repo/") {
			expected[name] = sum
		}
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("prepared configuration changed after preflight; prepare again before publishing")
	}
	for _, name := range sortedKeys(files) {
		if err := writePrivate(filepath.Join(snapshot, "repo", filepath.FromSlash(name)), files[name]); err != nil {
			return err
		}
	}
	return nil
}

func loadPublication(root string) (*prepareResult, layout, error) {
	var report prepareResult
	var spec layout
	raw, err := readArtifact(root, "prepare.json", maxStackSettingsBytes)
	if err != nil {
		return nil, spec, err
	}
	if json.Unmarshal(raw, &report) != nil || !report.Complete || !report.Validated || len(report.Groups) == 0 || len(report.Artifacts) == 0 {
		return nil, spec, fmt.Errorf("publish requires a completed, validated preparation with artifact checksums; prepare again with this version")
	}
	actual, err := preparedArtifacts(root, &report)
	if err != nil {
		return nil, spec, err
	}
	if !reflect.DeepEqual(actual, report.Artifacts) {
		return nil, spec, fmt.Errorf("prepared files changed; prepare again before publishing")
	}
	raw, err = readArtifact(root, "private/layout.json", maxStackSettingsBytes)
	if err != nil {
		return nil, spec, err
	}
	if json.Unmarshal(raw, &spec) != nil || spec.SourceStackID != report.SourceStackID || spec.ToolVersion != report.ToolVersion || spec.WorkerPoolID != report.WorkerPoolID || spec.RunnerImage != report.RunnerImage || len(spec.Groups) != len(report.Groups) {
		return nil, spec, fmt.Errorf("preparation layout does not match its report")
	}
	if _, err := checkSource(filepath.Join(root, "private/original.tfstate"), filepath.Join(root, "prepare.json")); err != nil {
		return nil, spec, err
	}
	original, _, err := readState(filepath.Join(root, "private/original.tfstate"))
	if err != nil {
		return nil, spec, err
	}
	for _, resource := range original.Resources {
		if resource.Type != "spacelift_stack" || resource.Mode != "managed" {
			continue
		}
		for _, instance := range resource.Instances {
			var attributes struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(instance.Attributes, &attributes) != nil {
				return nil, spec, fmt.Errorf("invalid stack attributes in source state")
			}
			if attributes.ID == spec.SourceStackID {
				return nil, spec, fmt.Errorf("source manages its own stack settings; API publication requires a reviewed configuration change for that resource")
			}
		}
	}
	groups := make([]preparedGroup, 0, len(spec.Groups))
	lineages := map[string]bool{original.Lineage: true}
	for i, g := range spec.Groups {
		if g.Name != report.Groups[i].Name || g.Module != report.Groups[i].Module {
			return nil, spec, fmt.Errorf("preparation group does not match its report")
		}
		child, _, err := readState(filepath.Join(root, report.Groups[i].StateFile))
		if err != nil {
			return nil, spec, err
		}
		if lineages[child.Lineage] || child.Lineage == "" || child.TerraformVersion != report.ToolVersion {
			return nil, spec, fmt.Errorf("invalid child state lineage or tool version")
		}
		lineages[child.Lineage] = true
		if err := verifyPartition(original, child, g.Module); err != nil {
			return nil, spec, err
		}
		groups = append(groups, preparedGroup{group: g})
	}
	parent, _, err := readState(filepath.Join(root, "private/states/parent.tfstate"))
	if err != nil {
		return nil, spec, err
	}
	if err := verifyRemainder(original, parent, groups); err != nil {
		return nil, spec, err
	}
	return &report, spec, nil
}

// Publication uses imports for the already-created children; no state is committed.
func publicationConfig(root, dest string, spec layout, report *prepareResult, repoID string, children []publishedChild) (*prepareResult, error) {
	if err := copyConfiguration(filepath.Join(root, "repo"), dest); err != nil {
		return nil, err
	}
	controller := filepath.Join(dest, "admin_split_controller.tf")
	if err := os.Remove(controller); err != nil {
		return nil, err
	}
	spec.Repository, spec.Branch, spec.VCSProvider, spec.VCSBlock = repoID, "main", "SPACELIFT", "spacelift_repo {}"
	var groups []preparedGroup
	for _, g := range spec.Groups {
		groups = append(groups, preparedGroup{group: g})
	}
	creates, err := renderController(dest, spec, groups)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(controller)
	if err != nil {
		return nil, err
	}
	f, diags := hclwrite.ParseConfig(raw, controller, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("invalid publication controller")
	}
	imports := map[string]string{}
	for _, child := range children {
		imports["spacelift_stack.admin_split_"+child.Group] = child.ID
	}
	for _, block := range f.Body().Blocks() {
		if block.Type() != "resource" || len(block.Labels()) != 2 || block.Labels()[0] != "spacelift_stack" {
			continue
		}
		address := strings.Join(block.Labels(), ".")
		id := imports[address]
		if id == "" {
			return nil, fmt.Errorf("child import missing from publication")
		}
		block.Body().RemoveAttribute("import_state_file")
		block.Body().SetAttributeValue("slug", cty.StringVal(id))
		// Import in provider 1.58 reads the OpenTofu workflow into these legacy
		// attributes. Use the same representation for API creation and HCL so the
		// first plan adopts the child without attempting a vendor migration.
		block.Body().SetAttributeValue("terraform_version", cty.StringVal(spec.ToolVersion))
		block.Body().SetAttributeValue("terraform_workflow_tool", cty.StringVal("OPEN_TOFU"))
		block.Body().SetAttributeValue("terraform_smart_sanitization", cty.BoolVal(true))
		for _, nested := range block.Body().Blocks() {
			if nested.Type() == "lifecycle" || nested.Type() == "opentofu" {
				block.Body().RemoveBlock(nested)
			}
		}
		// This provider-local flag has no remote value to read during import.
		// Ignoring its absent import value avoids an otherwise spurious update.
		lifecycle := block.Body().AppendNewBlock("lifecycle", nil)
		lifecycle.Body().SetAttributeRaw("ignore_changes", expressionTokens("[prevent_changes_when_locked]"))
		imp := f.Body().AppendNewBlock("import", nil)
		imp.Body().SetAttributeRaw("to", expressionTokens(address))
		imp.Body().SetAttributeValue("id", cty.StringVal(id))
	}
	if err := writePrivate(controller, f.Bytes()); err != nil {
		return nil, err
	}
	if err := writePrivate(filepath.Join(dest, ".gitignore"), []byte(".terraform/\n*.tfstate*\n*.tfplan\nprivate/\n")); err != nil {
		return nil, err
	}
	review := *report
	review.ControllerCreates = nil
	review.ControllerImports = imports
	for _, address := range creates {
		if imports[address] == "" {
			review.ControllerCreates = append(review.ControllerCreates, address)
		}
	}
	return &review, nil
}

func revisionFiles(dir, projectRoot string) ([]map[string]string, error) {
	files, err := configFiles(dir)
	if err != nil {
		return nil, err
	}
	var result []map[string]string
	for _, name := range sortedKeys(files) {
		result = append(result, map[string]string{"path": path.Join(projectRoot, name), "content": base64.StdEncoding.EncodeToString(files[name]), "action": "CREATE", "fileMode": "100644"})
	}
	return result, nil
}
