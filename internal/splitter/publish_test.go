package splitter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type publishTestAPI struct {
	server             *httptest.Server
	stack              map[string]any
	state              []byte
	uploads            map[string][]byte
	children           map[string]map[string]any
	writes             []string
	files              []map[string]string
	fail               string
	machine, busy      bool
	onRepoCreate       func()
	loseImportResponse bool
}

func publicationFixture(t *testing.T, validate bool) (string, string, *prepareResult, *publishTestAPI) {
	t.Helper()
	config, state, spec, tool := sourceFixture(t)
	root := filepath.Join(t.TempDir(), "prepared")
	report, err := prepare(config, state, spec, root, tool, validate)
	if err != nil {
		t.Fatal(err)
	}
	if !validate { // Unit preflight tests do not claim native configuration validation.
		report.Validated = true
		if err := writeJSON(filepath.Join(root, "prepare.json"), report); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	m := &publishTestAPI{state: raw, uploads: map[string][]byte{}, children: map[string]map[string]any{}}
	m.stack = map[string]any{
		"id": "existing-admin", "name": "Existing admin", "space": "root", "repository": "fixture-repo", "namespace": "", "branch": "main", "provider": "SPACELIFT", "projectRoot": "", "runnerImage": "", "workerPool": nil,
		"vcsIntegration": map[string]any{"id": "fixture-repo", "isDefault": false},
		"roleBindings":   []any{map[string]any{"role": map[string]any{"id": "role-space-admin", "slug": "space-admin"}, "space": map[string]any{"id": "root"}}},
		"vendorConfig":   map[string]any{"__typename": "StackConfigVendorOpenTofu", "version": report.ToolVersion, "openTofuWorkflowTool": "OPENTOFU", "workspace": "", "useSmartSanitization": true, "externalStateAccessEnabled": true, "concise": true, "enableCPUProfiling": false},
		"autodeploy":     true, "autoretry": true, "administrative": false, "description": "Existing description", "githubActionDeploy": false, "isDisabled": false, "labels": []string{"existing-label"},
		"localPreviewEnabled": true, "skipVendorBinaryDownload": false, "gitSparseCheckoutPaths": []string{}, "additionalProjectGlobs": []string{"shared/**"}, "protectFromDeletion": true, "repositoryURL": nil, "enableWellKnownSecretMasking": true, "enableSensitiveOutputUpload": false,
		"hooks": map[string][]string{"afterPlan": {}, "beforeInit": {}}, "lockedBy": nil, "lockNote": nil, "managesStateFile": true, "isLockedByTemplateDeployment": false, "allowedActions": []string{"STACK_MANAGED_STATE_IMPORT", "STACK_UPDATE", "STACK_LOCK", "STACK_UNLOCK"}, "blocker": nil, "stateIsTerminal": true, "attachedContexts": []any{},
	}
	m.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { m.serve(t, w, r) }))
	t.Cleanup(m.server.Close)
	return root, tool, report, m
}

func (m *publishTestAPI) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if strings.HasPrefix(r.URL.Path, "/storage/") {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("account credentials reached storage")
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write(m.state)
			return
		}
		if r.Method != http.MethodPut {
			t.Error("unexpected storage method")
		}
		raw, _ := io.ReadAll(r.Body)
		m.uploads[strings.TrimPrefix(r.URL.Path, "/storage/")] = raw
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Header.Get("Authorization") != "Bearer private-test-token" {
		t.Error("account token missing")
	}
	var request struct {
		Query     string                     `json:"query"`
		Variables map[string]json.RawMessage `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Error(err)
		return
	}
	str := func(key string) string { var v string; _ = json.Unmarshal(request.Variables[key], &v); return v }
	respond := func(field string, value any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{field: value}})
	}
	if strings.HasPrefix(request.Query, "mutation") && !strings.Contains(request.Query, "stateDownloadUrl") {
		operation := strings.Fields(request.Query)[1]
		m.writes = append(m.writes, operation)
		if m.fail != "" && strings.Contains(request.Query, m.fail) {
			_, _ = io.WriteString(w, `{"errors":[{"message":"private-response-secret"}]}`)
			return
		}
	}
	switch {
	case strings.Contains(request.Query, "viewer"):
		if m.machine {
			respond("viewer", nil)
		} else {
			respond("viewer", map[string]any{"id": "fixture-user", "admin": true, "actions": []any{}})
		}
	case request.Query == publicationStackQuery:
		if str("id") == "existing-admin" {
			respond("stack", m.stack)
		} else {
			respond("stack", m.children[str("id")])
		}
	case strings.Contains(request.Query, "AdminSplitterIdle"):
		items := []any{}
		if m.busy {
			items = append(items, map[string]any{"id": "pending-run", "finished": false})
		}
		respond("stack", map[string]any{"items": items})
	case strings.Contains(request.Query, "stateDownloadUrl"):
		respond("stateDownloadUrl", map[string]string{"url": m.server.URL + "/storage/current?signature=private-test-signature"})
	case strings.Contains(request.Query, "stateUploadUrl"):
		id := fmt.Sprint(len(m.uploads) + 1)
		respond("stateUploadUrl", map[string]any{"objectId": id, "url": m.server.URL + "/storage/" + id, "uploadHeaders": map[string]any{"entries": []map[string]string{{"key": "x-storage-header", "value": "storage-only"}}}})
	case strings.Contains(request.Query, "stackDisable"):
		if m.stack["isDisabled"] == true {
			t.Error("disabled an already disabled parent")
		}
		m.stack["isDisabled"] = true
		respond("stackDisable", map[string]string{"id": "existing-admin"})
	case strings.Contains(request.Query, "stackEnable"):
		m.stack["isDisabled"] = false
		respond("stackEnable", map[string]string{"id": "existing-admin"})
	case strings.Contains(request.Query, "stackUnlock"):
		m.stack["lockedBy"] = nil
		respond("stackUnlock", map[string]string{"id": "existing-admin"})
	case strings.Contains(request.Query, "stackLock"):
		m.stack["lockedBy"] = "fixture-user"
		m.stack["lockNote"] = str("note")
		respond("stackLock", map[string]string{"id": "existing-admin"})
	case strings.Contains(request.Query, "repoCreate"):
		if m.onRepoCreate != nil {
			m.onRepoCreate()
		}
		respond("repoCreate", map[string]string{"id": "published-repository"})
	case strings.Contains(request.Query, "revisionCreate"):
		var input struct {
			Files []map[string]string `json:"files"`
		}
		_ = json.Unmarshal(request.Variables["input"], &input)
		m.files = input.Files
		respond("revisionCreate", map[string]string{"sha": "published-commit"})
	case strings.Contains(request.Query, "stackCreate"):
		var input map[string]any
		_ = json.Unmarshal(request.Variables["input"], &input)
		if input["isDisabled"] != true || input["autodeploy"] != false {
			t.Error("child was not created disabled with autoapply off")
		}
		if _, ok := m.uploads[str("state")]; !ok {
			t.Error("child created without uploaded state")
		}
		input["id"] = str("slug")
		m.children[str("slug")] = input
		var pool any
		if input["workerPool"] != nil {
			pool = map[string]any{"id": input["workerPool"]}
		}
		respond("stackCreate", map[string]any{"id": str("slug"), "isDisabled": true, "workerPool": pool, "runnerImage": input["runnerImage"]})
	case strings.Contains(request.Query, "stackManagedStateImport"):
		if m.stack["isDisabled"] != true || m.stack["lockedBy"] == nil || len(m.children) != 2 {
			t.Error("parent imported before children were created and parent frozen")
		}
		m.state = m.uploads[str("state")]
		if m.loseImportResponse {
			// The service accepted the import, but its success response was lost.
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "private-response-secret")
			return
		}
		respond("stackManagedStateImport", nil)
	case strings.Contains(request.Query, "stackUpdate"):
		var input map[string]any
		_ = json.Unmarshal(request.Variables["input"], &input)
		if input["isDisabled"] != true || input["autodeploy"] != false {
			t.Error("unsafe parent update")
		}
		for _, key := range []string{"name", "description", "autoretry", "githubActionDeploy", "localPreviewEnabled", "protectFromDeletion", "enableWellKnownSecretMasking", "enableSensitiveOutputUpload"} {
			if !reflect.DeepEqual(input[key], m.stack[key]) {
				t.Errorf("parent setting %s was lost", key)
			}
		}
		for key, value := range input {
			m.stack[key] = value
		}
		m.stack["vcsIntegration"] = map[string]any{"id": input["repository"], "isDefault": false}
		if input["workerPool"] != nil {
			m.stack["workerPool"] = map[string]any{"id": input["workerPool"]}
		}
		for key, rawVendor := range input["vendorConfig"].(map[string]any) {
			vendor := rawVendor.(map[string]any)
			if key == "opentofu" {
				vendor["__typename"], vendor["openTofuWorkflowTool"] = "StackConfigVendorOpenTofu", vendor["workflowTool"]
			} else {
				vendor["__typename"], vendor["terraformWorkflowTool"] = "StackConfigVendorTerraform", vendor["workflowTool"]
			}
			delete(vendor, "workflowTool")
			m.stack["vendorConfig"] = vendor
		}
		respond("stackUpdate", map[string]string{"id": "existing-admin"})
	case strings.Contains(request.Query, "runTrigger"):
		if !strings.Contains(request.Query, "runType:PROPOSED") || str("commit") != "published-commit" {
			t.Error("publication did not request a proposed plan of its exact commit")
		}
		respond("runTrigger", map[string]string{"id": "review-run"})
	case strings.Contains(request.Query, "stackSetCurrentCommit"):
		if m.stack["isDisabled"] != true {
			t.Error("current commit changed after releasing parent")
		}
		respond("stackSetCurrentCommit", map[string]any{"id": "existing-admin", "trackedCommit": map[string]string{"hash": str("sha")}})
	default:
		t.Errorf("unexpected operation: %s", request.Query)
		http.Error(w, "unexpected", 400)
	}
}

func TestPublishPreflight(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(*publishTestAPI, string)
	}{
		{"preview", "", func(*publishTestAPI, string) {}},
		{"state changed", "state changed", func(m *publishTestAPI, _ string) { m.state = append(m.state, ' ') }},
		{"machine token", "machine sessions", func(m *publishTestAPI, _ string) { m.machine = true }},
		{"pending run", "unfinished runs", func(m *publishTestAPI, _ string) { m.busy = true }},
		{"foreign lock", "already locked", func(m *publishTestAPI, _ string) { m.stack["lockedBy"] = "someone-else" }},
		{"runtime hooks", "source hooks", func(m *publishTestAPI, _ string) { m.stack["hooks"] = map[string][]string{"afterPlan": {"custom"}} }},
		{"context", "attached contexts", func(m *publishTestAPI, _ string) {
			m.stack["attachedContexts"] = []any{map[string]string{"id": "context"}}
		}},
		{"file edited", "prepared files changed", func(_ *publishTestAPI, root string) {
			writeTest(t, filepath.Join(root, "repo", "added.tf"), "# changed\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, tool, _, m := publicationFixture(t, false)
			tc.change(m, root)
			result, err := publish(context.Background(), m.server.URL, root, tool, false, apiCredentials{Token: "private-test-token"}, m.server.Client())
			if tc.want == "" {
				if err != nil || result.Published || len(result.Children) != 2 {
					t.Fatalf("unexpected preview result: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v; want %s", err, tc.want)
			}
			if len(m.writes) != 0 {
				t.Fatal("preflight changed the account")
			}
			if _, err := os.Stat(filepath.Join(root, "private", "publish")); !os.IsNotExist(err) {
				t.Fatal("preview created a publication workspace")
			}
		})
	}
}

func TestPublicationPreservesPrivateRuntimeSettings(t *testing.T) {
	_, _, _, m := publicationFixture(t, false)
	m.stack["workerPool"] = map[string]any{"id": "private-pool"}
	m.stack["runnerImage"] = "registry.example/private/runner:fixed"
	m.stack["hooks"] = map[string][]string{"afterPlan": {"private custom hook"}}
	raw, _ := json.Marshal(map[string]any{"stack": m.stack})
	stack, err := stackSettingsObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	input, err := parentStackInput(stack)
	if err != nil {
		t.Fatal(err)
	}
	if input["workerPool"] != "private-pool" || input["runnerImage"] != "registry.example/private/runner:fixed" || !reflect.DeepEqual(input["afterPlan"], []string{"private custom hook"}) {
		t.Fatal("source runtime settings were lost")
	}
	delete(stack, "workerPool")
	if _, err := parentStackInput(stack); err == nil {
		t.Fatal("missing worker metadata silently selected public workers")
	}
}

func TestPublishNativeValidation(t *testing.T) {
	if os.Getenv("ADMIN_SPLITTER_VALIDATE_INTEGRATION") != "1" {
		t.Skip("enable native publication validation")
	}
	for _, failure := range []string{"", "stackManagedStateImport", "runTrigger", "lost import response"} {
		t.Run("failure="+failure, func(t *testing.T) {
			root, tool, report, m := publicationFixture(t, true)
			m.fail = failure
			if failure == "lost import response" {
				m.fail = ""
				m.loseImportResponse = true
			}
			m.stack["isDisabled"] = true
			// An editor may change prepared files while API writes are in flight.
			// Upload must keep using the checked and validated snapshot.
			m.onRepoCreate = func() {
				writeTest(t, filepath.Join(root, "repo", "main.tf"), "invalid HCL after validation {\n")
			}
			result, err := publish(context.Background(), m.server.URL, root, tool, true, apiCredentials{Token: "private-test-token"}, m.server.Client())
			var journal publicationJournal
			raw, readErr := os.ReadFile(filepath.Join(root, "private", "publish", "recovery.json"))
			if readErr != nil || json.Unmarshal(raw, &journal) != nil {
				t.Fatal("missing recovery journal")
			}
			if failure != "" {
				if err == nil || strings.Contains(err.Error(), "private-response-secret") || !journal.FailureFreezeSucceeded || m.stack["isDisabled"] != true {
					t.Fatalf("unsafe failure: %v", err)
				}
				if journal.Pending == "" {
					t.Fatal("lost uncertain step")
				}
				if m.loseImportResponse {
					want, _ := os.ReadFile(filepath.Join(root, "private", "states", "parent.tfstate"))
					if string(m.state) != string(want) || journal.Pending != "import reduced parent state" || journal.ParentStateObject == "" {
						t.Fatal("uncertain import was rolled back or not recorded")
					}
					if len(journal.Result.Children) != 2 || len(m.children) != 2 {
						t.Fatal("uncertain import lost the children needed for recovery")
					}
					attempts := 0
					for _, operation := range m.writes {
						if strings.HasPrefix(operation, "AdminSplitterParentImport(") {
							attempts++
						}
					}
					if attempts != 1 {
						t.Fatal("uncertain import was retried")
					}
				}
			} else {
				if err != nil || !result.Published || result.RunID != "review-run" || !journal.Result.Published {
					t.Fatalf("publication failed: %v", err)
				}
				want, _ := os.ReadFile(filepath.Join(root, "private", "states", "parent.tfstate"))
				if string(want) != string(m.state) {
					t.Fatal("wrong parent state uploaded")
				}
				for i, child := range result.Children {
					expected, _ := os.ReadFile(filepath.Join(root, report.Groups[i].StateFile))
					if string(expected) != string(m.uploads[fmt.Sprint(i+1)]) || !child.Created {
						t.Fatal("child upload changed state bytes")
					}
				}
				for _, f := range m.files {
					name := f["path"]
					raw, _ := base64.StdEncoding.DecodeString(f["content"])
					if strings.Contains(name, ".tfstate") || strings.Contains(name, "private/") || strings.Contains(string(raw), "import_state_file") {
						t.Fatal("state or state mount committed")
					}
					if strings.Contains(string(raw), "invalid HCL after validation") {
						t.Fatal("published an unchecked edit from the prepared directory")
					}
				}
			}
			for _, child := range m.children {
				if child["isDisabled"] != true {
					t.Fatal("child was enabled")
				}
			}
			before := len(m.writes)
			_, err = publish(context.Background(), m.server.URL, root, tool, true, apiCredentials{Token: "private-test-token"}, m.server.Client())
			if err == nil || len(m.writes) != before {
				t.Fatal("repeat publication made writes")
			}
		})
	}
}

func TestVerifyPublishedImports(t *testing.T) {
	prepared := filepath.Join(t.TempDir(), "review.json")
	if err := writeJSON(prepared, prepareResult{Complete: true, Validated: true, ToolVersion: "1.10.6", ControllerImports: map[string]string{"spacelift_stack.child": "child-id"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, changes string
		ok            bool
	}{
		{"matching", `{"address":"spacelift_stack.child","mode":"managed","change":{"actions":["no-op"],"importing":{"id":"child-id"}}}`, true},
		{"wrong id", `{"address":"spacelift_stack.child","mode":"managed","change":{"actions":["no-op"],"importing":{"id":"other"}}}`, false},
		{"import with update", `{"address":"spacelift_stack.child","mode":"managed","change":{"actions":["update"],"importing":{"id":"child-id"}}}`, false},
		{"missing", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"format_version":"1.2","terraform_version":"1.10.6","planned_values":{},"resource_changes":[` + tc.changes + `]}`)
			if err := verifyPlanJSON(raw, prepared, true); (err == nil) != tc.ok {
				t.Fatalf("unexpected verification: %v", err)
			}
			if tc.ok && verifyPlanJSON(raw, "", false) == nil {
				t.Fatal("import allowed without an explicit report")
			}
		})
	}
}

func TestStateUploadRejectsRedirectsAndCredentialHeaders(t *testing.T) {
	for _, mode := range []string{"redirect", "account header", "insecure URL"} {
		t.Run(mode, func(t *testing.T) {
			followed := false
			destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				followed = true
				t.Error("unsafe upload destination reached")
			}))
			defer destination.Close()
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/upload" {
					if r.Header.Get("Authorization") != "" {
						t.Error("account token reached storage")
					}
					http.Redirect(w, r, destination.URL+"/?private-signature", http.StatusTemporaryRedirect)
					return
				}
				u := server.URL + "/upload?private-signature"
				headers := []map[string]string{}
				if mode == "account header" {
					headers = append(headers, map[string]string{"key": "Authorization", "value": "private-secret"})
				}
				if mode == "insecure URL" {
					u = "http://insecure.example/?private-signature"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"stateUploadUrl": map[string]any{"objectId": "object", "url": u, "uploadHeaders": map[string]any{"entries": headers}}}})
			}))
			defer server.Close()
			api := publicationAPI{ctx: context.Background(), endpoint: server.URL, token: "private-token", client: server.Client()}
			_, err := api.upload([]byte("private-state-value"))
			if err == nil || followed {
				t.Fatal("unsafe upload succeeded")
			}
			for _, secret := range []string{"private-token", "private-secret", "private-signature", "private-state-value", destination.URL} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("upload error leaked private information")
				}
			}
		})
	}
}
