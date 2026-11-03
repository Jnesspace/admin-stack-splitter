package splitter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const fetchedStateFixture = "{\n  \"version\": 4, \"terraform_version\": \"1.10.0\", \"serial\": 7, \"lineage\": \"source-lineage\", \"resources\": [], \"outputs\": {\"secret\": {\"value\": \"private-state-marker\", \"sensitive\": true}}\n}\n"

func TestFetchSourcePreservesBytesAndWritesPrivateFiles(t *testing.T) {
	var server *httptest.Server
	settings := settingsJSON(t, map[string]any{"data": map[string]any{"stack": sourceSettingsFixture()}})
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download" {
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("account credentials leaked to state download")
			}
			_, _ = io.WriteString(w, fetchedStateFixture)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("account request missing bearer token")
		}
		var request struct {
			Query     string                     `json:"query"`
			Variables map[string]json.RawMessage `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Query == SourceStackSettingsQuery {
			_, _ = w.Write(settings)
			return
		}
		var input struct {
			StackID string `json:"stackId"`
		}
		if json.Unmarshal(request.Variables["input"], &input) != nil || input.StackID != "existing-admin" || !strings.Contains(request.Query, "stateDownloadUrl(input: $input)") {
			t.Error("state download was not requested for the selected stack")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"stateDownloadUrl": map[string]string{"url": server.URL + "/download?signature=private-url-marker"}}})
	}))
	defer server.Close()
	client := server.Client()
	client.Jar, _ = cookiejar.New(nil)
	account, _ := url.Parse(server.URL)
	client.Jar.SetCookies(account, []*http.Cookie{{Name: "session", Value: "private-cookie-marker"}})
	out := filepath.Join(t.TempDir(), "source")
	result, err := fetchSource(context.Background(), server.URL, "existing-admin", out, apiCredentials{Token: "test-token"}, client)
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.ReadFile(result.StatePath)
	if err != nil || string(state) != fetchedStateFixture {
		t.Fatal("downloaded state bytes changed")
	}
	savedSettings, err := os.ReadFile(result.SettingsPath)
	if err != nil || string(savedSettings) != string(settings) {
		t.Fatal("source settings were not saved exactly")
	}
	hash := sha256.Sum256(state)
	if result.SourceSHA256 != hex.EncodeToString(hash[:]) || result.SourceLineage != "source-lineage" || result.SourceSerial != 7 || result.ToolVersion != "1.10.0" || result.SourceStackID != "existing-admin" {
		t.Fatal("fetch metadata does not match source state")
	}
	for path, mode := range map[string]os.FileMode{out: 0700, result.StatePath: 0600, result.SettingsPath: 0600, filepath.Join(out, ".gitignore"): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("wrong private permissions for %s: %v", filepath.Base(path), err)
		}
	}
	if git, err := exec.LookPath("git"); err == nil {
		gitCommand := func(args ...string) *exec.Cmd {
			return exec.Command(git, append([]string{"-c", "core.excludesFile=" + os.DevNull, "-C", filepath.Dir(out)}, args...)...)
		}
		if output, err := gitCommand("init", "--quiet").CombinedOutput(); err != nil {
			t.Fatalf("initialize test repository: %v: %s", err, output)
		}
		for _, path := range []string{result.StatePath, result.SettingsPath} {
			if output, err := gitCommand("check-ignore", "--quiet", "--", path).CombinedOutput(); err != nil {
				t.Fatalf("snapshot can be staged with Git: %v: %s", err, output)
			}
		}
	}
	printed, _ := json.Marshal(result)
	if strings.Contains(string(printed), "private-") || strings.Contains(string(printed), "test-token") {
		t.Fatal("fetch result exposes secrets")
	}
	if _, err := fetchSource(context.Background(), server.URL, "existing-admin", out, apiCredentials{Token: "test-token"}, client); err == nil {
		t.Fatal("overwrote an existing snapshot directory")
	}
}

func TestFetchRejectsInvalidStateWithoutLeavingOutput(t *testing.T) {
	for _, state := range []string{
		`private-state-marker`, `null`, `{"version":4,"terraform_version":"1.10.0"}`,
		`{"version":4,"terraform_version":"1.10.0","lineage":"lineage","resources":[{"instances":[{"deposed":"old"}]}]}`,
	} {
		var server *httptest.Server
		server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/download" {
				_, _ = io.WriteString(w, state)
				return
			}
			var request struct {
				Query string `json:"query"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request.Query == SourceStackSettingsQuery {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"stack": sourceSettingsFixture()}})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"stateDownloadUrl": map[string]string{"url": server.URL + "/download"}}})
			}
		}))
		out := filepath.Join(t.TempDir(), "source")
		_, err := fetchSource(context.Background(), server.URL, "existing-admin", out, apiCredentials{Token: "token"}, server.Client())
		server.Close()
		if err == nil || strings.Contains(err.Error(), "private-state-marker") {
			t.Fatalf("invalid state accepted or exposed: %v", err)
		}
		if _, err := os.Lstat(out); !os.IsNotExist(err) {
			t.Fatal("invalid state left a partial snapshot directory")
		}
	}
}

func TestDownloadStateRejectsUnsafeURLsAndResponses(t *testing.T) {
	for _, downloadURL := range []string{"", "http://example.test/private-url-marker", "https://user:private-url-marker@example.test", "https://example.test/#private-url-marker"} {
		if _, err := downloadState(context.Background(), downloadURL, nil); err == nil || strings.Contains(err.Error(), "private-url-marker") {
			t.Fatalf("unsafe download URL accepted or exposed: %v", err)
		}
	}
	for _, status := range []int{http.StatusForbidden, http.StatusTemporaryRedirect} {
		var redirected atomic.Bool
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/redirected" {
				redirected.Store(true)
			}
			w.Header().Set("Location", "/redirected?signature=private-url-marker")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, "private-state-marker")
		}))
		_, err := downloadState(context.Background(), server.URL+"/download?signature=private-url-marker", server.Client())
		server.Close()
		if err == nil || redirected.Load() || strings.Contains(err.Error(), "private-") {
			t.Fatalf("download failure accepted, followed or exposed: %v", err)
		}
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(maxStateDownloadBytes+1))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if _, err := downloadState(context.Background(), server.URL, server.Client()); err == nil || !strings.Contains(err.Error(), "100 MiB") {
		t.Fatalf("oversized download was not rejected: %v", err)
	}
}
