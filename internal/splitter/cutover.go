package splitter

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"

	"github.com/jnesspace/admin-stack-splitter/docs"
)

func renderCutover(preparedDir string, report *prepareResult) ([]byte, error) {
	sizes := make(map[string]int64, len(report.Groups))
	oversized := false
	for _, g := range report.Groups {
		info, err := os.Stat(filepath.Join(preparedDir, filepath.FromSlash(g.StateFile)))
		if err != nil {
			return nil, fmt.Errorf("read child snapshot size: %w", err)
		}
		sizes[g.Name] = info.Size()
		oversized = oversized || info.Size() > 2_000_000
	}
	var out strings.Builder
	fmt.Fprintln(&out, "# Your prepared split")
	fmt.Fprintln(&out)
	if !report.Complete || !report.Validated {
		fmt.Fprintln(&out, "**STOP: this preparation has not passed configuration validation. Prepare again with validation enabled before performing any cutover step.**")
	} else {
		fmt.Fprintln(&out, "Local preparation and configuration validation passed. Follow each account step manually; live plans still need verification.")
	}
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Run the commands from your working directory containing `./admin-splitter`. Copy this setup block into that shell:")
	fmt.Fprintln(&out)
	setup := "SPLIT_STACK_ID=" + shellQuote(report.SourceStackID) + "\nSPLIT_PREPARED=" + shellQuote(filepath.ToSlash(preparedDir)) + "\n"
	fence := "```"
	for strings.Contains(setup, fence) {
		fence += "`"
	}
	fmt.Fprintf(&out, "%sbash\n%s%s\n\n", fence, setup, fence)
	pool := report.WorkerPoolID
	if pool == "" {
		pool = "Public worker pool (inherited from source)"
	}
	fmt.Fprintln(&out, "| Setting | Value |")
	fmt.Fprintln(&out, "| --- | --- |")
	fmt.Fprintf(&out, "| Original parent stack ID | %s |\n", markdownCell(report.SourceStackID))
	fmt.Fprintf(&out, "| Inherited worker pool | %s |\n", markdownCell(pool))
	fmt.Fprintf(&out, "| OpenTofu version | %s |\n", markdownCell(report.ToolVersion))
	fmt.Fprintf(&out, "| Original resource instances | %d |\n", report.SourceInstances)
	fmt.Fprintf(&out, "| Original instances retained by parent | %d |\n", report.ParentInstances)
	fmt.Fprintf(&out, "| Child admins | %d |\n", len(report.Groups))
	fmt.Fprintf(&out, "| Parent state to import | %s |\n", markdownCell(filepath.Join(preparedDir, "private", "states", "parent.tfstate")))
	fmt.Fprintf(&out, "| Configuration to publish | %s |\n", markdownCell(filepath.Join(preparedDir, "repo")))
	fmt.Fprintf(&out, "| Original state backup | %s |\n\n", markdownCell(filepath.Join(preparedDir, "private", "original.tfstate")))
	fmt.Fprintln(&out, "## Child state mounts")
	fmt.Fprintln(&out)
	if oversized {
		fmt.Fprintln(&out, "**Delivery needed before cutover:** at least one child snapshot exceeds 2 MB. Arrange approved private delivery to its exact runner path before importing parent state. See the state-delivery step below.")
		fmt.Fprintln(&out)
	}
	fmt.Fprintln(&out, "For each file within the mount limit, create a write-only mounted file on the original parent with mode `600`. Larger files need private delivery as described below. The relative mount path determines the runner path `/mnt/workspace/<relative mount path>`.")
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "| Child stack name | Instances | State bytes | Local state file | Relative mount path |")
	fmt.Fprintln(&out, "| --- | ---: | ---: | --- | --- |")
	for _, g := range report.Groups {
		fmt.Fprintf(&out, "| %s | %d | %d | %s | %s |\n", markdownCell(childStackName(report.SourceStackID, g)), g.Instances, sizes[g.Name],
			markdownCell(filepath.Join(preparedDir, filepath.FromSlash(g.StateFile))), markdownCell(g.Name+".tfstate"))
	}
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "## Child configuration paths")
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "After cutover, make future edits in these paths. Each path is relative to the contents of the configuration directory you publish.")
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "| Child stack name | Configuration path |")
	fmt.Fprintln(&out, "| --- | --- |")
	for _, g := range report.Groups {
		fmt.Fprintf(&out, "| %s | %s |\n", markdownCell(childStackName(report.SourceStackID, g)), markdownCell(strings.TrimPrefix(g.ConfigDirectory, "repo/")))
	}
	out.WriteString("\n---\n\n")
	out.WriteString(docs.Cutover)
	return []byte(out.String()), nil
}

func childStackName(parentID string, g groupResult) string {
	if g.StackName != "" {
		return g.StackName
	}
	return parentID + "-" + g.Name
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func markdownCell(value string) string {
	value = strings.NewReplacer("\r", `\r`, "\n", `\n`, "\t", `\t`).Replace(value)
	return "<code>" + strings.ReplaceAll(html.EscapeString(value), "|", "&#124;") + "</code>"
}
