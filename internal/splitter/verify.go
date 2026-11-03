package splitter

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func verifyPlan(path, prepared string, allowCreate bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var plan struct {
		Format        string          `json:"format_version"`
		Version       string          `json:"terraform_version"`
		PlannedValues json.RawMessage `json:"planned_values"`
		StateValues   json.RawMessage `json:"values"`
		Errored       bool            `json:"errored"`
		Complete      *bool           `json:"complete"`
		Changes       []struct {
			Address string `json:"address"`
			Mode    string `json:"mode"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if json.Unmarshal(raw, &plan) != nil || !strings.HasPrefix(plan.Format, "1.") || plan.Version == "" {
		return fmt.Errorf("require a supported tofu/terraform show -json plan")
	}
	var planned map[string]json.RawMessage
	if len(plan.StateValues) > 0 || json.Unmarshal(plan.PlannedValues, &planned) != nil || planned == nil {
		return fmt.Errorf("require saved plan JSON with planned_values, not a state snapshot")
	}
	if plan.Errored || (plan.Complete != nil && !*plan.Complete) {
		return fmt.Errorf("plan is errored or incomplete")
	}
	allowed := map[string]bool{}
	if allowCreate {
		if prepared == "" {
			return fmt.Errorf("--allow-controller-create requires --prepared")
		}
		raw, err := os.ReadFile(prepared)
		if err != nil {
			return err
		}
		var report prepareResult
		if json.Unmarshal(raw, &report) != nil || !report.Complete || !report.Validated {
			return fmt.Errorf("preparation is incomplete or has not passed configuration validation")
		}
		for _, address := range report.ControllerCreates {
			allowed[address] = true
		}
	}
	for _, change := range plan.Changes {
		actions := change.Change.Actions
		if len(actions) != 1 {
			return fmt.Errorf("plan changes %s; replacements and mixed actions are refused", change.Address)
		}
		switch actions[0] {
		case "no-op":
			continue
		case "read":
			if change.Mode == "data" {
				continue
			}
		case "create":
			if allowed[change.Address] {
				continue
			}
		}
		return fmt.Errorf("plan changes existing resource %s (%s)", change.Address, strings.Join(actions, ","))
	}
	return nil
}
