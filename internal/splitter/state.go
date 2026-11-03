package splitter

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

type stateFile struct {
	Version          int                        `json:"version"`
	TerraformVersion string                     `json:"terraform_version"`
	Serial           uint64                     `json:"serial"`
	Lineage          string                     `json:"lineage"`
	Outputs          map[string]json.RawMessage `json:"outputs"`
	Resources        []stateResource            `json:"resources"`
}

type stateResource struct {
	Module    string          `json:"module"`
	Mode      string          `json:"mode"`
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Provider  string          `json:"provider"`
	Instances []stateInstance `json:"instances"`
}

type stateInstance struct {
	IndexKey   any             `json:"index_key"`
	Attributes json.RawMessage `json:"attributes"`
	Deposed    string          `json:"deposed"`
	Sensitive  json.RawMessage `json:"sensitive_attributes"`
}

func readState(path string) (*stateFile, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read state: %w", err)
	}
	state, err := parseState(raw)
	if err != nil {
		return nil, nil, err
	}
	return state, raw, nil
}

func parseState(raw []byte) (*stateFile, error) {
	var state stateFile
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("invalid state JSON")
	}
	if state.Version != 4 || state.Lineage == "" || state.TerraformVersion == "" {
		return nil, fmt.Errorf("require an unencrypted version-4 state snapshot with lineage and tool version")
	}
	for _, r := range state.Resources {
		for _, i := range r.Instances {
			if i.Deposed != "" {
				return nil, fmt.Errorf("state contains a deposed instance; finish its replacement before splitting")
			}
		}
	}
	return &state, nil
}

func within(module, prefix string) bool {
	return module == prefix || strings.HasPrefix(module, prefix+".")
}

func (s *stateFile) summary() any {
	counts := map[string]int{}
	for _, r := range s.Resources {
		key := r.Module
		if key == "" {
			key = "<root>"
		}
		counts[key] += len(r.Instances)
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var modules []map[string]any
	total := 0
	for _, key := range keys {
		modules = append(modules, map[string]any{"module": key, "instances": counts[key]})
		total += counts[key]
	}
	return map[string]any{"version": s.Version, "tool_version": s.TerraformVersion, "serial": s.Serial, "lineage": s.Lineage, "instances": total, "outputs": len(s.Outputs), "total_including_outputs": total + len(s.Outputs), "modules": modules}
}
