package splitter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const publicationStackQuery = `query AdminSplitterPublicationStack($id: ID!) {
  stack(id: $id) {
    id name space repository namespace branch provider projectRoot runnerImage
    workerPool { id } vcsIntegration { id isDefault }
    roleBindings { role { id slug } space { id } }
    vendorConfig {
      __typename
      ... on StackConfigVendorOpenTofu {
        version openTofuWorkflowTool: workflowTool workspace useSmartSanitization
        externalStateAccessEnabled concise enableCPUProfiling
      }
      ... on StackConfigVendorTerraform {
        version terraformWorkflowTool: workflowTool workspace useSmartSanitization externalStateAccessEnabled
      }
    }
    autodeploy autoretry administrative description githubActionDeploy isDisabled labels
    localPreviewEnabled skipVendorBinaryDownload gitSparseCheckoutPaths additionalProjectGlobs
    protectFromDeletion repositoryURL enableWellKnownSecretMasking enableSensitiveOutputUpload
    hooks { afterApply beforeApply afterInit beforeInit afterPlan beforePlan afterPerform
      beforePerform afterDestroy afterRun beforeDestroy }
    lockedBy lockNote managesStateFile isLockedByTemplateDeployment
    blocker { id } stateIsTerminal attachedContexts { id }
  }
}`

type publicationAPI struct {
	ctx             context.Context
	endpoint, token string
	client          *http.Client
}

func (a publicationAPI) call(operation, query string, vars any, field string, result any) error {
	raw, err := accountRequest(a.ctx, a.endpoint, a.token, operation, query, vars, a.client)
	if err != nil {
		return err
	}
	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || len(envelope.Data[field]) == 0 {
		return fmt.Errorf("invalid %s response", operation)
	}
	if result != nil && json.Unmarshal(envelope.Data[field], result) != nil {
		return fmt.Errorf("invalid %s response", operation)
	}
	return nil
}

func (a publicationAPI) stack(id string) (map[string]json.RawMessage, error) {
	var stack map[string]json.RawMessage
	err := a.call("publication stack settings", publicationStackQuery, map[string]string{"id": id}, "stack", &stack)
	return stack, err
}

func stackString(s map[string]json.RawMessage, key string) string {
	var v string
	_ = json.Unmarshal(s[key], &v)
	return v
}

func stackBool(s map[string]json.RawMessage, key string) bool {
	var v bool
	_ = json.Unmarshal(s[key], &v)
	return v
}

func parentStackInput(stack map[string]json.RawMessage) (map[string]any, error) {
	input := map[string]any{}
	for _, key := range strings.Fields(`name repository namespace branch provider projectRoot runnerImage space
autodeploy autoretry administrative description githubActionDeploy isDisabled labels
localPreviewEnabled skipVendorBinaryDownload gitSparseCheckoutPaths additionalProjectGlobs
protectFromDeletion repositoryURL enableWellKnownSecretMasking enableSensitiveOutputUpload`) {
		var value any
		if json.Unmarshal(stack[key], &value) != nil {
			return nil, fmt.Errorf("source settings omit required field %s", key)
		}
		input[key] = value
	}
	var hooks map[string][]string
	if json.Unmarshal(stack["hooks"], &hooks) != nil {
		return nil, fmt.Errorf("source settings omit hooks")
	}
	for key, value := range hooks {
		input[key] = value
	}
	var pool *struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(stack["workerPool"], &pool) != nil {
		return nil, fmt.Errorf("source settings omit worker pool")
	}
	input["workerPool"] = nil
	if pool != nil {
		input["workerPool"] = pool.ID
	}
	var vcs struct {
		ID        string `json:"id"`
		IsDefault bool   `json:"isDefault"`
	}
	if json.Unmarshal(stack["vcsIntegration"], &vcs) != nil || vcs.ID == "" {
		return nil, fmt.Errorf("source settings omit VCS integration")
	}
	input["vcsIntegrationId"] = vcs.ID
	if vcs.IsDefault {
		input["vcsIntegrationId"] = nil
	}
	var vendor map[string]any
	if json.Unmarshal(stack["vendorConfig"], &vendor) != nil {
		return nil, fmt.Errorf("source settings omit vendor configuration")
	}
	key := "opentofu"
	workflow := vendor["openTofuWorkflowTool"]
	if vendor["__typename"] == "StackConfigVendorTerraform" {
		key, workflow = "terraform", vendor["terraformWorkflowTool"]
	}
	if (key == "opentofu" && workflow != "OPENTOFU") || (key == "terraform" && workflow != "OPEN_TOFU") {
		return nil, fmt.Errorf("publish requires the standard OpenTofu workflow")
	}
	delete(vendor, "__typename")
	delete(vendor, "openTofuWorkflowTool")
	delete(vendor, "terraformWorkflowTool")
	vendor["workflowTool"] = workflow
	input["vendorConfig"] = map[string]any{key: vendor}
	return input, nil
}

func (a publicationAPI) idle(id string) error {
	// Include old queued runs and tasks, not just the latest page or current blocker.
	for _, field := range []string{"runs", "tasks", "proposedRuns"} {
		before := ""
		for page := 0; ; page++ {
			if page >= 100 {
				return fmt.Errorf("run history exceeds the publication preflight limit")
			}
			var stack *struct {
				Items []struct {
					ID       string `json:"id"`
					Finished bool   `json:"finished"`
				} `json:"items"`
			}
			vars := map[string]any{"id": id, "before": nil}
			if before != "" {
				vars["before"] = before
			}
			query := `query AdminSplitterIdle($id: ID!, $before: ID) { stack(id: $id) { items: ` + field + `(before: $before) { id finished } } }`
			if err := a.call("unfinished run check", query, vars, "stack", &stack); err != nil {
				return err
			}
			if stack == nil {
				return fmt.Errorf("source stack was not returned")
			}
			if len(stack.Items) == 0 {
				break
			}
			for _, run := range stack.Items {
				if !run.Finished {
					return fmt.Errorf("source has unfinished runs or tasks; finish or discard them before publishing")
				}
			}
			next := stack.Items[len(stack.Items)-1].ID
			if next == "" || next == before {
				return fmt.Errorf("run history pagination did not advance")
			}
			before = next
		}
	}
	return nil
}

func (a publicationAPI) state(id string) ([]byte, error) {
	var result struct {
		URL string `json:"url"`
	}
	if err := a.call("state download URL request", `mutation AdminSplitterPublicationDownload($input: StateDownloadUrlInput!) { stateDownloadUrl(input:$input) { url } }`, map[string]any{"input": map[string]string{"stackId": id}}, "stateDownloadUrl", &result); err != nil {
		return nil, err
	}
	return downloadState(a.ctx, result.URL, a.client)
}

func (a publicationAPI) upload(raw []byte) (string, error) {
	if len(raw) == 0 || len(raw) > maxStateDownloadBytes {
		return "", fmt.Errorf("state must be nonempty and at most 100 MiB")
	}
	var upload struct {
		ObjectID string `json:"objectId"`
		URL      string `json:"url"`
		Headers  struct {
			Entries []struct{ Key, Value string } `json:"entries"`
		} `json:"uploadHeaders"`
	}
	if err := a.call("state upload URL request", `mutation AdminSplitterStateUpload { stateUploadUrl { objectId url uploadHeaders { entries { key value } } } }`, nil, "stateUploadUrl", &upload); err != nil {
		return "", err
	}
	u, err := url.Parse(upload.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || upload.ObjectID == "" {
		return "", fmt.Errorf("invalid state upload URL response")
	}
	req, err := http.NewRequestWithContext(a.ctx, http.MethodPut, upload.URL, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("cannot create state upload request")
	}
	// Signed storage headers only; never copy the account token, cookies or client jar.
	for _, h := range upload.Headers.Entries {
		key := strings.ToLower(h.Key)
		if key == "authorization" || key == "cookie" || key == "host" || strings.ContainsAny(h.Key+h.Value, "\r\n") {
			return "", fmt.Errorf("unsafe state upload header")
		}
		req.Header.Set(h.Key, h.Value)
	}
	response, err := boundedHTTPClient(a.client, 2*time.Minute).Do(req)
	if err != nil {
		return "", fmt.Errorf("state upload failed; no response details are printed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("state upload returned HTTP %d", response.StatusCode)
	}
	return upload.ObjectID, nil
}

func (a publicationAPI) stackAction(action, id, note string) error {
	if action == "stackDisable" || action == "stackEnable" {
		current, err := a.stack(id)
		if err != nil {
			return err
		}
		if current == nil {
			return fmt.Errorf("stack was not returned while checking pause status")
		}
		if _, ok := current["isDisabled"]; !ok {
			return fmt.Errorf("stack response omitted pause status")
		}
		if stackBool(current, "isDisabled") == (action == "stackDisable") {
			return nil
		}
	}
	var result struct {
		ID string `json:"id"`
	}
	query := `mutation AdminSplitterStackAction($id: ID!) { ` + action + `(id:$id) { id } }`
	vars := map[string]string{"id": id}
	if action == "stackLock" {
		query = `mutation AdminSplitterStackAction($id: ID!, $note: String) { stackLock(id:$id, note:$note) { id } }`
		vars["note"] = note
	}
	if err := a.call(action, query, vars, action, &result); err != nil {
		return err
	}
	if result.ID != id {
		return fmt.Errorf("%s did not return the requested stack", action)
	}
	return nil
}
