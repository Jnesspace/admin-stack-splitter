package splitter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sourceCheckFixture(t *testing.T) (string, string, []byte, map[string]any) {
	t.Helper()
	dir := t.TempDir()
	statePath, preparedPath := filepath.Join(dir, "fresh.tfstate"), filepath.Join(dir, "prepare.json")
	raw := []byte(`{"version":4,"terraform_version":"1.10.0","serial":7,"lineage":"source-lineage","resources":[],"outputs":{"secret":{"sensitive":true,"value":"private-state-value"}}}`)
	hash := sha256.Sum256(raw)
	report := map[string]any{
		"complete": true, "validated": true, "source_stack_id": "existing-admin",
		"source_lineage": "source-lineage", "source_serial": 7, "tool_version": "1.10.0",
		"source_sha256": hex.EncodeToString(hash[:]),
	}
	writeTest(t, statePath, string(raw))
	if err := writeJSON(preparedPath, report); err != nil {
		t.Fatal(err)
	}
	return statePath, preparedPath, raw, report
}

func assertSourceCheckReadOnly(t *testing.T, statePath, preparedPath string, stateBefore, reportBefore []byte) {
	t.Helper()
	for path, before := range map[string][]byte{statePath: stateBefore, preparedPath: reportBefore} {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("check-source changed an input file: %v", err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(statePath))
	if err != nil || len(entries) != 2 {
		t.Fatalf("check-source created an output file: %v", err)
	}
}

func TestCheckSourceMatchesValidatedPreparationWithoutWrites(t *testing.T) {
	statePath, preparedPath, stateBefore, report := sourceCheckFixture(t)
	reportBefore, err := os.ReadFile(preparedPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := checkSource(statePath, preparedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Matches || result.SourceStackID != "existing-admin" || result.SourceLineage != "source-lineage" || result.SourceSerial != 7 || result.SourceSHA256 != report["source_sha256"] {
		t.Fatalf("unexpected source comparison result: %+v", result)
	}
	printed, err := json.Marshal(result)
	if err != nil || strings.Contains(string(printed), "private-state-value") {
		t.Fatal("source comparison exposed a state value")
	}
	assertSourceCheckReadOnly(t, statePath, preparedPath, stateBefore, reportBefore)
}

func TestCheckSourceRejectsChangedSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name, old, replacement string
	}{
		{"serial", `"serial":7`, `"serial":8`},
		{"lineage", "source-lineage", "another-lineage"},
		{"tool version", "1.10.0", "1.10.1"},
		{"same serial changed value", "private-state-value", "new-private-state-value"},
		{"same values changed bytes", `"version":4`, `"version": 4`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statePath, preparedPath, raw, _ := sourceCheckFixture(t)
			changed := []byte(strings.Replace(string(raw), tc.old, tc.replacement, 1))
			writeTest(t, statePath, string(changed))
			reportBefore, err := os.ReadFile(preparedPath)
			if err != nil {
				t.Fatal(err)
			}
			result, err := checkSource(statePath, preparedPath)
			if err == nil || result != nil || !strings.Contains(err.Error(), "state changed") || !strings.Contains(err.Error(), "prepare again") || strings.Contains(err.Error(), "private-state-value") {
				t.Fatalf("changed snapshot was accepted or exposed: %v", err)
			}
			assertSourceCheckReadOnly(t, statePath, preparedPath, changed, reportBefore)
		})
	}
}

func TestCheckSourceRequiresValidatedSourceMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		value       any
	}{
		{"incomplete", "complete", false},
		{"diagnostic preparation", "validated", false},
		{"empty stack", "source_stack_id", ""},
		{"whitespace stack", "source_stack_id", "existing admin"},
		{"wrong type stack", "source_stack_id", 10},
		{"empty lineage", "source_lineage", ""},
		{"whitespace lineage", "source_lineage", " source-lineage"},
		{"control lineage", "source_lineage", "source\x00lineage"},
		{"null serial", "source_serial", nil},
		{"negative serial", "source_serial", -1},
		{"wrong type serial", "source_serial", "7"},
		{"empty version", "tool_version", ""},
		{"invalid version", "tool_version", "private-report-value"},
		{"malformed version", "tool_version", "1.10.0-"},
		{"empty hash", "source_sha256", ""},
		{"short hash", "source_sha256", strings.Repeat("a", 63)},
		{"invalid hash", "source_sha256", strings.Repeat("g", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statePath, preparedPath, raw, report := sourceCheckFixture(t)
			report[tc.field] = tc.value
			if err := writeJSON(preparedPath, report); err != nil {
				t.Fatal(err)
			}
			reportBefore, err := os.ReadFile(preparedPath)
			if err != nil {
				t.Fatal(err)
			}
			result, err := checkSource(statePath, preparedPath)
			if err == nil || result != nil || strings.Contains(err.Error(), "private-report-value") {
				t.Fatalf("invalid preparation metadata was accepted or exposed: %v", err)
			}
			assertSourceCheckReadOnly(t, statePath, preparedPath, raw, reportBefore)
		})
	}
	statePath, preparedPath, _, report := sourceCheckFixture(t)
	delete(report, "source_serial")
	if err := writeJSON(preparedPath, report); err != nil {
		t.Fatal(err)
	}
	if _, err := checkSource(statePath, preparedPath); err == nil {
		t.Fatal("accepted missing source serial")
	}
}

func TestCheckSourceRejectsWrongReportAndInvalidState(t *testing.T) {
	for _, rawReport := range []string{`private-report-value`, `null`, `[]`, `{}`, `{"complete":true,"validated":true}`, fetchedStateFixture} {
		statePath, preparedPath, stateBefore, _ := sourceCheckFixture(t)
		writeTest(t, preparedPath, rawReport)
		if result, err := checkSource(statePath, preparedPath); err == nil || result != nil || strings.Contains(err.Error(), "private-report-value") {
			t.Fatalf("invalid report accepted or exposed: %v", err)
		}
		assertSourceCheckReadOnly(t, statePath, preparedPath, stateBefore, []byte(rawReport))
	}
	statePath, preparedPath, _, _ := sourceCheckFixture(t)
	writeTest(t, statePath, "private-state-value")
	if _, err := checkSource(statePath, preparedPath); err == nil || strings.Contains(err.Error(), "private-state-value") {
		t.Fatalf("invalid state accepted or exposed: %v", err)
	}
	if _, err := checkSource(statePath, ""); err == nil {
		t.Fatal("accepted a missing report path")
	}
	if _, err := checkSource("", preparedPath); err == nil {
		t.Fatal("accepted a missing snapshot path")
	}
}

func TestCheckSourceAcceptsZeroSerialAndPrereleaseVersion(t *testing.T) {
	statePath, preparedPath, raw, report := sourceCheckFixture(t)
	raw = bytes.Replace(raw, []byte(`"serial":7`), []byte(`"serial":0`), 1)
	raw = bytes.Replace(raw, []byte("1.10.0"), []byte("1.11.0-beta1+build.1"), 1)
	hash := sha256.Sum256(raw)
	report["source_serial"], report["tool_version"], report["source_sha256"] = 0, "1.11.0-beta1+build.1", strings.ToUpper(hex.EncodeToString(hash[:]))
	writeTest(t, statePath, string(raw))
	if err := writeJSON(preparedPath, report); err != nil {
		t.Fatal(err)
	}
	result, err := checkSource(statePath, preparedPath)
	if err != nil || !result.Matches || result.SourceSerial != 0 || result.SourceSHA256 != hex.EncodeToString(hash[:]) {
		t.Fatalf("valid zero serial/prerelease metadata rejected: %v", err)
	}
}
