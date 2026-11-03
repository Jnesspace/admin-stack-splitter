package splitter

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCustomerFetchInspectPrepareWorkflow(t *testing.T) {
	config, snapshot, _, tool, _ := migrationKitFixture(t)
	state, original, err := readState(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	settings := sourcePrepareSettings(t, state.TerraformVersion)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/download" {
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" {
				t.Error("state download must use GET without the API credential")
			}
			_, _ = w.Write(original)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			t.Error("unexpected API method or path")
		}
		var request struct {
			Query     string                     `json:"query"`
			Variables map[string]json.RawMessage `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid test request", http.StatusBadRequest)
			return
		}
		if strings.Contains(request.Query, "apiKeyUser") {
			var id, secret string
			_ = json.Unmarshal(request.Variables["id"], &id)
			_ = json.Unmarshal(request.Variables["secret"], &secret)
			if id != "test-key-id" || secret != "test-private-key-secret" || r.Header.Get("Authorization") != "" {
				t.Error("CLI did not supply API credentials through authentication variables")
			}
			_, _ = io.WriteString(w, `{"data":{"apiKeyUser":{"jwt":"test-private-jwt"}}}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-private-jwt" {
			t.Error("CLI did not use the authenticated session")
		}
		switch {
		case request.Query == SourceStackSettingsQuery:
			var id string
			_ = json.Unmarshal(request.Variables["id"], &id)
			if id != "existing-admin" {
				t.Error("CLI queried the wrong source stack")
			}
			_, _ = w.Write(append(append([]byte(`{"data":`), settings...), '}'))
		case strings.Contains(request.Query, "stateDownloadUrl"):
			var input struct {
				StackID string `json:"stackId"`
			}
			_ = json.Unmarshal(request.Variables["input"], &input)
			if input.StackID != "existing-admin" {
				t.Error("CLI requested state from the wrong stack")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"stateDownloadUrl": map[string]string{"url": "https://" + r.Host + "/download?signature=test-private-signature"}}})
		default:
			t.Error("unexpected GraphQL operation")
			http.Error(w, "unexpected operation", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	previousClient := http.DefaultClient
	http.DefaultClient = server.Client()
	t.Cleanup(func() { http.DefaultClient = previousClient })
	t.Setenv("SPACELIFT_API_KEY_ENDPOINT", server.URL)
	t.Setenv("SPACELIFT_API_KEY_ID", "test-key-id")
	t.Setenv("SPACELIFT_API_KEY_SECRET", "test-private-key-secret")
	t.Setenv("SPACELIFT_API_TOKEN", "")
	work := t.TempDir()
	source := filepath.Join(work, "source")
	var fetched bytes.Buffer
	if err := Run([]string{"fetch", "--stack", "existing-admin", "--out", source}, &fetched); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"test-private-key-secret", "test-private-jwt", "test-private-signature", "/download"} {
		if strings.Contains(fetched.String(), secret) {
			t.Fatal("fetch output exposed credentials or a signed URL")
		}
	}
	saved, err := os.ReadFile(filepath.Join(source, "admin.tfstate"))
	if err != nil || !bytes.Equal(saved, original) {
		t.Fatalf("fetch did not preserve the source snapshot: %v", err)
	}
	var inspected bytes.Buffer
	if err := Run([]string{"inspect", "--config", config, "--state", filepath.Join(source, "admin.tfstate")}, &inspected); err != nil {
		t.Fatal(err)
	}
	var preview map[string]json.RawMessage
	if err := json.Unmarshal(inspected.Bytes(), &preview); err != nil {
		t.Fatalf("inspect did not emit JSON: %v", err)
	}
	for _, mode := range []string{"downloaded settings", "direct settings"} {
		t.Run(mode, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "prepared")
			validate := os.Getenv("ADMIN_SPLITTER_VALIDATE_INTEGRATION") == "1"
			args := []string{"prepare", "--config", config, "--state", filepath.Join(source, "admin.tfstate"), "--out", out, "--tool", tool, "--validate=" + strconv.FormatBool(validate)}
			if mode == "downloaded settings" {
				t.Setenv("SPACELIFT_API_KEY_ID", "")
				t.Setenv("SPACELIFT_API_KEY_SECRET", "")
				args = append(args, "--source-stack", filepath.Join(source, "source-stack.json"))
			} else {
				args = append(args, "--stack", "existing-admin")
			}
			before := requests.Load()
			var prepared bytes.Buffer
			if err := Run(args, &prepared); err != nil {
				t.Fatal(err)
			}
			if mode == "downloaded settings" && requests.Load() != before {
				t.Fatal("offline preparation made an account request")
			}
			var report prepareResult
			if err := json.Unmarshal(prepared.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if !report.Complete || report.Validated != validate || report.SourceInstances != 29 || report.ParentInstances != 5 || len(report.Groups) != 2 || report.WorkerPoolID != "original-private-pool" {
				t.Fatalf("downloaded source did not produce the expected split: %+v", report)
			}
		})
	}
}

func TestFetchCommandRequiresInputs(t *testing.T) {
	t.Setenv("SPACELIFT_API_KEY_ENDPOINT", "")
	for _, args := range [][]string{
		{"fetch"},
		{"fetch", "--stack", "existing-admin", "--out", "source"},
		{"fetch", "--endpoint", "https://account.example", "--out", "source"},
		{"fetch", "--stack", "existing-admin", "--endpoint", "https://account.example"},
	} {
		if err := Run(args, io.Discard); err == nil || !strings.Contains(err.Error(), "fetch requires") {
			t.Fatalf("missing fetch inputs were not rejected: %v", err)
		}
	}
	if err := Run([]string{"fetch", "--help"}, io.Discard); err != nil {
		t.Fatalf("fetch help failed: %v", err)
	}
}
