package splitter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxStateDownloadBytes = 100 << 20

type fetchResult struct {
	SourceStackID string `json:"source_stack_id"`
	StatePath     string `json:"state_path"`
	SettingsPath  string `json:"settings_path"`
	SourceLineage string `json:"source_lineage"`
	SourceSerial  uint64 `json:"source_serial"`
	SourceSHA256  string `json:"source_sha256"`
	ToolVersion   string `json:"tool_version"`
}

func fetchSource(ctx context.Context, endpoint, stackID, out string, credentials apiCredentials, client *http.Client) (*fetchResult, error) {
	if strings.TrimSpace(stackID) == "" || out == "" {
		return nil, fmt.Errorf("fetch requires a stack ID and a new output directory")
	}
	if _, err := os.Lstat(out); err == nil || !os.IsNotExist(err) {
		return nil, fmt.Errorf("fetch output directory must not exist and its parent must be accessible")
	}
	token, err := authenticate(ctx, endpoint, credentials, client)
	if err != nil {
		return nil, err
	}
	settings, err := readStackSettingsWithClient(ctx, endpoint, stackID, token, client)
	if err != nil {
		return nil, err
	}
	raw, err := accountRequest(ctx, endpoint, token, "state download URL request", `mutation AdminSplitterStateDownload($input: StateDownloadUrlInput!) {
  stateDownloadUrl(input: $input) { url }
}`, map[string]any{"input": map[string]string{"stackId": stackID}}, client)
	if err != nil {
		return nil, err
	}
	var response struct {
		Data struct {
			Download struct {
				URL string `json:"url"`
			} `json:"stateDownloadUrl"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return nil, fmt.Errorf("invalid state download URL response")
	}
	stateRaw, err := downloadState(ctx, response.Data.Download.URL, client)
	if err != nil {
		return nil, err
	}
	state, err := parseState(stateRaw)
	if err != nil {
		return nil, err
	}
	if err := newDirectory(out); err != nil {
		return nil, fmt.Errorf("cannot create a new private fetch directory; check its parent and choose an unused path")
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(out)
		}
	}()
	hash := sha256.Sum256(stateRaw)
	result := &fetchResult{
		SourceStackID: stackID, StatePath: filepath.Join(out, "admin.tfstate"),
		SettingsPath: filepath.Join(out, "source-stack.json"), SourceLineage: state.Lineage,
		SourceSerial: state.Serial, SourceSHA256: hex.EncodeToString(hash[:]), ToolVersion: state.TerraformVersion,
	}
	if err := writePrivate(filepath.Join(out, ".gitignore"), []byte("*\n")); err != nil {
		return nil, fmt.Errorf("cannot protect the snapshot directory from Git staging; check directory permissions")
	}
	if err := writePrivate(result.StatePath, stateRaw); err != nil {
		return nil, fmt.Errorf("cannot write the private state snapshot; check disk space and directory permissions")
	}
	if err := writePrivate(result.SettingsPath, settings); err != nil {
		return nil, fmt.Errorf("cannot write the source settings snapshot; check disk space and directory permissions")
	}
	complete = true
	return result, nil
}

func downloadState(ctx context.Context, downloadURL string, client *http.Client) ([]byte, error) {
	parsed, err := url.Parse(downloadURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, fmt.Errorf("state download did not return a valid HTTPS URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("cannot create state download request")
	}
	// The signed URL carries its own authorization; account credentials must not follow it.
	bounded := boundedHTTPClient(client, 2*time.Minute)
	response, err := bounded.Do(request)
	if err != nil {
		return nil, fmt.Errorf("state download failed; check connectivity and fetch a fresh download URL")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("state download returned HTTP %d; check state access and retry fetch", response.StatusCode)
	}
	if response.ContentLength > maxStateDownloadBytes {
		return nil, fmt.Errorf("state exceeds the 100 MiB download limit; export it privately and prepare from local files")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxStateDownloadBytes+1))
	if err != nil || len(raw) > maxStateDownloadBytes {
		return nil, fmt.Errorf("state download could not be read within the 100 MiB size limit")
	}
	return raw, nil
}
