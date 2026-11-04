package splitter

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

func displayFetch(out io.Writer, result *fetchResult) error {
	var text strings.Builder
	fmt.Fprintf(&text, "Source snapshot downloaded for %s.\n\nOpenTofu version: %s\nState: %s\nSettings: %s\n", result.SourceStackID, result.ToolVersion, result.StatePath, result.SettingsPath)
	fmt.Fprintf(&text, "\nNext: install OpenTofu %s, then preview the split:\n  ./admin-splitter inspect --state %s --config ./generated\n", result.ToolVersion, shellQuote(result.StatePath))
	_, err := io.WriteString(out, text.String())
	return err
}

func displayState(out io.Writer, state *stateFile, statePath string) error {
	var text strings.Builder
	fmt.Fprintf(&text, "Source state\n\nOpenTofu version: %s\nResource instances: %d\nOutputs: %d\nState serial: %d\n", state.TerraformVersion, moduleInstances(state, ""), len(state.Outputs), state.Serial)
	fmt.Fprintf(&text, "\nNext: preview the child groups using the applied configuration:\n  ./admin-splitter inspect --state %s --config ./generated\n", shellQuote(statePath))
	_, err := io.WriteString(out, text.String())
	return err
}

func displayPreview(out io.Writer, preview *splitPreview) error {
	var text strings.Builder
	fmt.Fprintf(&text, "Proposed split\n\nSource resource instances: %d\nRemaining in parent: %d\nChild admins: %d\n\n", preview.SourceInstances, preview.ParentInstances, len(preview.Groups))
	displayGroups(&text, preview.Groups)
	text.WriteString("\nNext: review these boundaries, then run prepare with the same configuration and snapshot.\n")
	_, err := io.WriteString(out, text.String())
	return err
}

func displayPreparation(out io.Writer, result *prepareResult, directory string) error {
	var text strings.Builder
	switch {
	case !result.Complete:
		text.WriteString("Preparation incomplete. NOT ready for cutover.\n")
	case !result.Validated:
		text.WriteString("Diagnostic split prepared without validation. NOT ready for cutover.\n")
	default:
		text.WriteString("Split prepared and validated locally.\n")
	}
	pool := result.WorkerPoolID
	if pool == "" {
		pool = "Public"
	}
	fmt.Fprintf(&text, "\nSource admin: %s\nWorker pool: %s\nOpenTofu version: %s\nSource resource instances: %d\nRemaining in parent: %d\nChild admins: %d\n\n", result.SourceStackID, pool, result.ToolVersion, result.SourceInstances, result.ParentInstances, len(result.Groups))
	displayGroups(&text, result.Groups)
	fmt.Fprintf(&text, "\nConfiguration: %s\nPrivate state: %s\nReport: %s\n", filepath.Join(directory, "repo"), filepath.Join(directory, "private", "states"), filepath.Join(directory, "prepare.json"))
	if result.Complete && result.Validated {
		fmt.Fprintf(&text, "\nNext: open %s for API publication and plan review.\n", filepath.Join(directory, "CUTOVER.md"))
	} else {
		text.WriteString("\nNext: prepare again into a new directory with validation enabled before cutover.\n")
	}
	_, err := io.WriteString(out, text.String())
	return err
}

func displaySourceCheck(out io.Writer, result *sourceCheckResult) error {
	_, err := fmt.Fprintf(out, "Source state matches the prepared snapshot.\n\nSource admin: %s\nState serial: %d\n\nNext: confirm the live stack is still frozen and its settings are unchanged, then continue in CUTOVER.md.\n", result.SourceStackID, result.SourceSerial)
	return err
}

func displayPlanCheck(out io.Writer, allowController bool) error {
	message := "Plan check passed: no managed-resource changes.\n"
	if allowController {
		message = "Plan check passed: managed-resource changes are limited to the recorded controller additions.\n"
	}
	_, err := io.WriteString(out, message+"\nNext: review output changes and continue the manual steps in CUTOVER.md.\n")
	return err
}

func displayGroups(out io.Writer, groups []groupResult) {
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "Child\tResource instances\tModule")
	for _, group := range groups {
		fmt.Fprintf(table, "%s\t%d\t%s\n", group.Name, group.Instances, group.Module)
	}
	_ = table.Flush()
}
