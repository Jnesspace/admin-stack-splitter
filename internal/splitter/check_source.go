package splitter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"
)

type sourceCheckResult struct {
	Matches       bool   `json:"matches"`
	SourceStackID string `json:"source_stack_id"`
	SourceLineage string `json:"source_lineage"`
	SourceSerial  uint64 `json:"source_serial"`
	SourceSHA256  string `json:"source_sha256"`
}

func checkSource(statePath, preparedPath string) (*sourceCheckResult, error) {
	if statePath == "" || preparedPath == "" {
		return nil, fmt.Errorf("check-source requires --state and --prepared")
	}
	rawReport, err := os.ReadFile(preparedPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read preparation report; provide prepare.json from a completed, validated preparation")
	}
	var report struct {
		Complete      bool    `json:"complete"`
		Validated     bool    `json:"validated"`
		SourceStackID string  `json:"source_stack_id"`
		SourceLineage string  `json:"source_lineage"`
		SourceSerial  *uint64 `json:"source_serial"`
		SourceSHA256  string  `json:"source_sha256"`
		ToolVersion   string  `json:"tool_version"`
	}
	invalidReport := fmt.Errorf("require a completed, validated preparation report with source stack, lineage, serial, tool version and SHA-256 metadata; prepare again from fresh inputs")
	if json.Unmarshal(rawReport, &report) != nil || !report.Complete || !report.Validated || report.SourceSerial == nil {
		return nil, invalidReport
	}
	for _, value := range []string{report.SourceStackID, report.SourceLineage} {
		if value == "" || strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
			return nil, invalidReport
		}
	}
	validVersion, _ := regexp.MatchString(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`, report.ToolVersion)
	expectedHash, err := hex.DecodeString(report.SourceSHA256)
	if !validVersion || err != nil || len(expectedHash) != sha256.Size {
		return nil, invalidReport
	}
	state, rawState, err := readState(statePath)
	if err != nil {
		return nil, fmt.Errorf("read fresh source snapshot: %w", err)
	}
	hash := sha256.Sum256(rawState)
	if state.Lineage != report.SourceLineage || state.Serial != *report.SourceSerial || state.TerraformVersion != report.ToolVersion || !bytes.Equal(hash[:], expectedHash) {
		return nil, fmt.Errorf("source state changed since preparation; keep the admin frozen, fetch fresh inputs and prepare again")
	}
	return &sourceCheckResult{
		Matches: true, SourceStackID: report.SourceStackID, SourceLineage: state.Lineage,
		SourceSerial: state.Serial, SourceSHA256: hex.EncodeToString(hash[:]),
	}, nil
}
