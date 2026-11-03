package splitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// SourceStackSettingsQuery excludes secrets and captures settings needed by child admins.
const SourceStackSettingsQuery = `query AdminSplitterSourceStack($id: ID!) {
  stack(id: $id) {
    id name space repository namespace branch provider projectRoot runnerImage
    workerPool { id }
    vcsIntegration { id isDefault }
    roleBindings { role { id slug } space { id } }
    vendorConfig {
      __typename
      ... on StackConfigVendorOpenTofu { version openTofuWorkflowTool: workflowTool }
      ... on StackConfigVendorTerraform { version terraformWorkflowTool: workflowTool }
    }
  }
}`

const maxStackSettingsBytes = 1 << 20

func stackSettingsObject(raw []byte) (map[string]json.RawMessage, error) {
	var envelope struct {
		Stack  json.RawMessage   `json:"stack"`
		Data   json.RawMessage   `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if len(raw) > maxStackSettingsBytes || json.Unmarshal(raw, &envelope) != nil {
		return nil, fmt.Errorf("invalid stack settings response")
	}
	if len(envelope.Errors) > 0 {
		return nil, fmt.Errorf("stack settings query failed; check account and stack access")
	}
	if len(envelope.Data) > 0 {
		if len(envelope.Stack) > 0 {
			return nil, fmt.Errorf("ambiguous stack settings response")
		}
		var data struct {
			Stack json.RawMessage `json:"stack"`
		}
		if json.Unmarshal(envelope.Data, &data) != nil {
			return nil, fmt.Errorf("invalid stack settings response")
		}
		envelope.Stack = data.Stack
	}
	var stack map[string]json.RawMessage
	if len(envelope.Stack) == 0 || json.Unmarshal(envelope.Stack, &stack) != nil || stack == nil {
		return nil, fmt.Errorf("source stack was not returned; check account and stack access")
	}
	return stack, nil
}

func layoutFromStackSettings(raw []byte) (layout, error) {
	var result layout
	stack, err := stackSettingsObject(raw)
	if err != nil {
		return result, err
	}
	for _, field := range []struct {
		name string
		out  *string
	}{
		{"id", &result.SourceStackID}, {"space", &result.SpaceID},
		{"repository", &result.Repository}, {"branch", &result.Branch},
		{"provider", &result.VCSProvider},
	} {
		if json.Unmarshal(stack[field.name], field.out) != nil || strings.TrimSpace(*field.out) == "" {
			return layout{}, fmt.Errorf("stack settings require a nonempty %s", field.name)
		}
	}
	for _, field := range []struct {
		name string
		out  *string
	}{
		{"projectRoot", &result.ProjectRoot}, {"runnerImage", &result.RunnerImage},
	} {
		if json.Unmarshal(stack[field.name], field.out) != nil {
			return layout{}, fmt.Errorf("stack settings must include %s as a string or null", field.name)
		}
	}
	poolRaw, present := stack["workerPool"]
	if !present {
		return layout{}, fmt.Errorf("stack settings must include workerPool; omitted settings cannot select public workers")
	}
	var pool *struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(poolRaw, &pool) != nil || (pool != nil && strings.TrimSpace(pool.ID) == "") {
		return layout{}, fmt.Errorf("workerPool must be null or an object with a nonempty id")
	}
	if pool != nil {
		result.WorkerPoolID = pool.ID
	}
	var integration struct {
		ID        string `json:"id"`
		IsDefault *bool  `json:"isDefault"`
	}
	if json.Unmarshal(stack["vcsIntegration"], &integration) != nil || strings.TrimSpace(integration.ID) == "" || integration.IsDefault == nil {
		return layout{}, fmt.Errorf("stack settings require vcsIntegration.id and vcsIntegration.isDefault")
	}
	switch result.VCSProvider {
	case "SPACELIFT":
		if integration.ID != result.Repository {
			return layout{}, fmt.Errorf("native repository and VCS integration IDs differ; configure the layout manually")
		}
		result.VCSBlock = "spacelift_repo {}"
	case "GITHUB", "GITHUB_ENTERPRISE":
		if result.VCSProvider == "GITHUB" && *integration.IsDefault {
			break
		}
		var namespace string
		if json.Unmarshal(stack["namespace"], &namespace) != nil || strings.TrimSpace(namespace) == "" {
			return layout{}, fmt.Errorf("custom GitHub integration requires the source namespace")
		}
		result.VCSBlock = fmt.Sprintf("github_enterprise {\n  id = %s\n  namespace = %s\n}", quote(integration.ID), quote(namespace))
	default:
		return layout{}, fmt.Errorf("source VCS provider is unsupported by settings discovery; configure the layout manually")
	}
	var vendor struct {
		Type             string `json:"__typename"`
		OpenTofuWorkflow string `json:"openTofuWorkflowTool"`
		TerraformTool    string `json:"terraformWorkflowTool"`
	}
	if json.Unmarshal(stack["vendorConfig"], &vendor) != nil ||
		!((vendor.Type == "StackConfigVendorOpenTofu" && vendor.OpenTofuWorkflow == "OPENTOFU") ||
			(vendor.Type == "StackConfigVendorTerraform" && vendor.TerraformTool == "OPEN_TOFU")) {
		return layout{}, fmt.Errorf("source stack must use the standard OpenTofu workflow")
	}
	var bindings []struct {
		Role struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"role"`
		Space struct {
			ID string `json:"id"`
		} `json:"space"`
	}
	if json.Unmarshal(stack["roleBindings"], &bindings) != nil {
		return layout{}, fmt.Errorf("stack settings must include roleBindings")
	}
	if len(bindings) != 1 || bindings[0].Space.ID != result.SpaceID || bindings[0].Role.Slug != "space-admin" || strings.TrimSpace(bindings[0].Role.ID) == "" {
		return layout{}, fmt.Errorf("source settings discovery requires exactly one space-admin role binding in the stack's own space; review permissions and configure the layout manually")
	}
	result.RoleID = bindings[0].Role.ID
	return result, nil
}

func readStackSettings(ctx context.Context, endpoint, stackID, token string) ([]byte, error) {
	return readStackSettingsWithClient(ctx, endpoint, stackID, token, http.DefaultClient)
}

func readStackSettingsWithClient(ctx context.Context, endpoint, stackID, token string, client *http.Client) ([]byte, error) {
	if strings.TrimSpace(stackID) == "" || strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return nil, fmt.Errorf("stack ID and a valid API token are required")
	}
	raw, err := accountRequest(ctx, endpoint, token, "stack settings", SourceStackSettingsQuery, map[string]string{"id": stackID}, client)
	if err != nil {
		return nil, err
	}
	settings, err := layoutFromStackSettings(raw)
	if err != nil {
		return nil, err
	}
	if settings.SourceStackID != stackID {
		return nil, fmt.Errorf("stack settings response does not match the requested stack")
	}
	return raw, nil
}
