package splitter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

func sourceSettingsFixture() map[string]any {
	return map[string]any{
		"id": "existing-admin", "name": "Existing admin", "space": "management",
		"repository": "migration-repo", "namespace": "", "branch": "main",
		"provider": "SPACELIFT", "projectRoot": "generated", "runnerImage": nil,
		"workerPool":     map[string]any{"id": "private-pool"},
		"vcsIntegration": map[string]any{"id": "migration-repo", "isDefault": false},
		"roleBindings": []any{
			map[string]any{"role": map[string]any{"id": "admin-role", "slug": "space-admin"}, "space": map[string]any{"id": "management"}},
		},
		"vendorConfig": map[string]any{"__typename": "StackConfigVendorOpenTofu", "version": "latest", "openTofuWorkflowTool": "OPENTOFU"},
	}
}

func settingsJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestLayoutFromStackSettingsEnvelopes(t *testing.T) {
	for _, envelope := range []string{"MCP", "GraphQL"} {
		t.Run(envelope, func(t *testing.T) {
			value := map[string]any{"stack": sourceSettingsFixture()}
			if envelope == "GraphQL" {
				value = map[string]any{"data": value}
			}
			got, err := layoutFromStackSettings(settingsJSON(t, value))
			if err != nil {
				t.Fatal(err)
			}
			if got.SourceStackID != "existing-admin" || got.SpaceID != "management" || got.RoleID != "admin-role" ||
				got.Repository != "migration-repo" || got.Branch != "main" || got.ProjectRoot != "generated" ||
				got.WorkerPoolID != "private-pool" || got.VCSProvider != "SPACELIFT" || got.VCSBlock != "spacelift_repo {}" {
				t.Fatalf("settings were not inherited: %+v", got)
			}
			if got.ToolVersion != "" {
				t.Fatal("source settings pinned a version instead of using the state snapshot")
			}
		})
	}
}

func TestLayoutFromStackSettingsRejectsIncompleteOrUnsupportedSettings(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
		want   string
	}{
		{"missing pool", func(s map[string]any) { delete(s, "workerPool") }, "workerPool"},
		{"missing private pool ID", func(s map[string]any) { s["workerPool"] = map[string]any{} }, "workerPool"},
		{"invalid pool", func(s map[string]any) { s["workerPool"] = "private-pool" }, "workerPool"},
		{"missing runner image", func(s map[string]any) { delete(s, "runnerImage") }, "runnerImage"},
		{"invalid runner image", func(s map[string]any) { s["runnerImage"] = 123 }, "runnerImage"},
		{"missing project root", func(s map[string]any) { delete(s, "projectRoot") }, "projectRoot"},
		{"missing integration", func(s map[string]any) { delete(s, "vcsIntegration") }, "vcsIntegration"},
		{"missing integration default", func(s map[string]any) { s["vcsIntegration"] = map[string]any{"id": "migration-repo"} }, "vcsIntegration"},
		{"null integration default", func(s map[string]any) { s["vcsIntegration"] = map[string]any{"id": "migration-repo", "isDefault": nil} }, "vcsIntegration"},
		{"wrong native integration", func(s map[string]any) { s["vcsIntegration"].(map[string]any)["id"] = "another-repo" }, "IDs differ"},
		{"unsupported VCS", func(s map[string]any) { s["provider"] = "AZURE_DEVOPS" }, "unsupported"},
		{"wrong workflow", func(s map[string]any) { s["vendorConfig"].(map[string]any)["openTofuWorkflowTool"] = "CUSTOM" }, "OpenTofu"},
		{"Terraform workflow", func(s map[string]any) {
			s["vendorConfig"] = map[string]any{"__typename": "StackConfigVendorTerraform", "terraformWorkflowTool": "TERRAFORM_FOSS"}
		}, "OpenTofu"},
		{"missing role bindings", func(s map[string]any) { delete(s, "roleBindings") }, "roleBindings"},
		{"wrong role scope", func(s map[string]any) {
			s["roleBindings"].([]any)[0].(map[string]any)["space"] = map[string]any{"id": "root"}
		}, "own space"},
		{"reader only", func(s map[string]any) {
			s["roleBindings"].([]any)[0].(map[string]any)["role"] = map[string]any{"id": "reader-role", "slug": "space-reader"}
		}, "space-admin"},
		{"ambiguous admin", func(s map[string]any) {
			s["roleBindings"] = append(s["roleBindings"].([]any), map[string]any{"role": map[string]any{"id": "another-role", "slug": "space-admin"}, "space": map[string]any{"id": "management"}})
		}, "exactly one"},
		{"additional root admin", func(s map[string]any) {
			s["roleBindings"] = append(s["roleBindings"].([]any), map[string]any{"role": map[string]any{"id": "admin-role", "slug": "space-admin"}, "space": map[string]any{"id": "root"}})
		}, "review permissions"},
		{"additional same-space reader", func(s map[string]any) {
			s["roleBindings"] = append(s["roleBindings"].([]any), map[string]any{"role": map[string]any{"id": "reader-role", "slug": "space-reader"}, "space": map[string]any{"id": "management"}})
		}, "review permissions"},
		{"additional external reader", func(s map[string]any) {
			s["roleBindings"] = append(s["roleBindings"].([]any), map[string]any{"role": map[string]any{"id": "reader-role", "slug": "space-reader"}, "space": map[string]any{"id": "shared-services"}})
		}, "review permissions"},
		{"custom role", func(s map[string]any) {
			s["roleBindings"].([]any)[0].(map[string]any)["role"] = map[string]any{"id": "custom-role", "slug": "migration-admin"}
		}, "configure the layout manually"},
		{"empty role bindings", func(s map[string]any) { s["roleBindings"] = []any{} }, "exactly one"},
		{"null role bindings", func(s map[string]any) { s["roleBindings"] = nil }, "exactly one"},
	}
	for _, field := range []string{"id", "space", "repository", "branch", "provider"} {
		cases = append(cases, struct {
			name   string
			change func(map[string]any)
			want   string
		}{"missing " + field, func(s map[string]any) { delete(s, field) }, field})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := sourceSettingsFixture()
			tc.change(s)
			_, err := layoutFromStackSettings(settingsJSON(t, map[string]any{"stack": s}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v; wanted error containing %q", err, tc.want)
			}
		})
	}
}

func TestLayoutFromStackSettingsPublicWorkersAndCustomRunner(t *testing.T) {
	s := sourceSettingsFixture()
	s["workerPool"] = nil
	s["runnerImage"] = "registry.example.test/admin:stable"
	s["projectRoot"] = nil
	s["vendorConfig"] = map[string]any{"__typename": "StackConfigVendorTerraform", "version": "1.8.0", "terraformWorkflowTool": "OPEN_TOFU"}
	got, err := layoutFromStackSettings(settingsJSON(t, map[string]any{"stack": s}))
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkerPoolID != "" || got.RunnerImage != s["runnerImage"] || got.ProjectRoot != "" || got.ToolVersion != "" {
		t.Fatalf("public pool or source runner settings changed: %+v", got)
	}
}

func TestLayoutFromStackSettingsGitHubIntegration(t *testing.T) {
	s := sourceSettingsFixture()
	s["provider"] = "GITHUB"
	s["namespace"] = "example-org"
	s["vcsIntegration"] = map[string]any{"id": "managed-github", "isDefault": true}
	got, err := layoutFromStackSettings(settingsJSON(t, map[string]any{"stack": s}))
	if err != nil || got.VCSBlock != "" || got.VCSProvider != "GITHUB" {
		t.Fatalf("default GitHub mapping: %+v, %v", got, err)
	}
	integration := "custom\"${var.token}\\integration"
	namespace := "org\n%{ if true }${file(\"private\")}"
	s["namespace"] = namespace
	s["vcsIntegration"] = map[string]any{"id": integration, "isDefault": false}
	for _, provider := range []string{"GITHUB", "GITHUB_ENTERPRISE"} {
		s["provider"] = provider
		got, err := layoutFromStackSettings(settingsJSON(t, map[string]any{"stack": s}))
		if err != nil {
			t.Fatal(err)
		}
		f, diagnostics := hclsyntax.ParseConfig([]byte(got.VCSBlock), "vcs.tf", hcl.InitialPos)
		if diagnostics.HasErrors() {
			t.Fatalf("invalid VCS block: %s", diagnostics)
		}
		blocks := f.Body.(*hclsyntax.Body).Blocks
		if len(blocks) != 1 || blocks[0].Type != "github_enterprise" {
			t.Fatal("custom GitHub integration rendered the wrong block")
		}
		for key, expected := range map[string]string{"id": integration, "namespace": namespace} {
			value, diagnostics := blocks[0].Body.Attributes[key].Expr.Value(nil)
			if diagnostics.HasErrors() || value.AsString() != expected {
				t.Fatalf("%s was not preserved as a literal: %s", key, diagnostics)
			}
		}
	}
	delete(s, "namespace")
	if _, err := layoutFromStackSettings(settingsJSON(t, map[string]any{"stack": s})); err == nil {
		t.Fatal("accepted a custom integration without namespace")
	}
}

func TestReadStackSettingsUsesVariablesAndChecksStackIdentity(t *testing.T) {
	stackID := "admin\") { unexpectedField } #"
	s := sourceSettingsFixture()
	s["id"] = stackID
	response := settingsJSON(t, map[string]any{"data": map[string]any{"stack": s}})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected request metadata")
		}
		var request struct {
			Query     string            `json:"query"`
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Query != SourceStackSettingsQuery || strings.Contains(request.Query, stackID) || request.Variables["id"] != stackID {
			t.Error("stack ID was not passed solely through GraphQL variables")
		}
		_, _ = w.Write(response)
	}))
	defer server.Close()
	for _, suffix := range []string{"", "/", "/graphql", "/graphql/"} {
		got, err := readStackSettingsWithClient(context.Background(), server.URL+suffix, stackID, "test-token", server.Client())
		if err != nil || string(got) != string(response) {
			t.Fatalf("read %s: %v", suffix, err)
		}
	}
}

func TestReadStackSettingsRejectsUnsafeOrInvalidResponses(t *testing.T) {
	wrong := sourceSettingsFixture()
	wrong["id"] = "another-admin"
	for _, tc := range []struct {
		name   string
		status int
		body   []byte
	}{
		{"wrong stack", http.StatusOK, settingsJSON(t, map[string]any{"data": map[string]any{"stack": wrong}})},
		{"GraphQL error", http.StatusOK, []byte(`{"errors":[{"message":"private-error-marker"}],"data":{"stack":null}}`)},
		{"not found", http.StatusOK, []byte(`{"data":{"stack":null}}`)},
		{"malformed", http.StatusOK, []byte(`private-error-marker`)},
		{"HTTP failure", http.StatusForbidden, []byte(`private-error-marker`)},
		{"oversized", http.StatusOK, []byte(strings.Repeat("x", maxStackSettingsBytes+1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write(tc.body)
			}))
			defer server.Close()
			_, err := readStackSettingsWithClient(context.Background(), server.URL, "existing-admin", "private-token-marker", server.Client())
			if err == nil {
				t.Fatal("accepted invalid source settings")
			}
			if strings.Contains(err.Error(), "private-error-marker") || strings.Contains(err.Error(), "private-token-marker") {
				t.Fatal("error exposed response body or authentication")
			}
		})
	}
}

func TestReadStackSettingsRefusesRedirects(t *testing.T) {
	var followed atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/graphql" {
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
			return
		}
		followed.Store(true)
		_, _ = io.WriteString(w, "private-error-marker")
	}))
	defer server.Close()
	_, err := readStackSettingsWithClient(context.Background(), server.URL, "existing-admin", "test-token", server.Client())
	if err == nil || followed.Load() {
		t.Fatal("redirect was followed or treated as a successful settings response")
	}
}

func TestReadStackSettingsRequiresHTTPSAndHonorsCancellation(t *testing.T) {
	for _, endpoint := range []string{"http://example.test", "https://user:private-token-marker@example.test", "https://example.test/path", "https://example.test/graphql?token=private-token-marker", "https://example.test/#private-token-marker"} {
		if _, err := readStackSettings(context.Background(), endpoint, "existing-admin", "test-token"); err == nil || strings.Contains(err.Error(), "private-token-marker") {
			t.Fatalf("invalid endpoint was accepted or exposed: %v", err)
		}
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := readStackSettingsWithClient(ctx, server.URL, "existing-admin", "test-token", server.Client()); err == nil {
		t.Fatal("request ignored its deadline")
	}
	if time.Since(started) > time.Second {
		t.Fatal("request did not return promptly after cancellation")
	}
}
