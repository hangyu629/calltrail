package model

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadRules reads a rules file: either {"rules": [...]} or a bare array. Rules without an id get
// "r1", "r2", ...; rules that match nothing by construction (no field set) are an error.
func LoadRules(path string) ([]Rule, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseRules(b)
}

func ParseRules(b []byte) ([]Rule, error) {
	var wrapped struct {
		Rules []Rule `json:"rules"`
	}
	var rs []Rule
	if err := json.Unmarshal(b, &wrapped); err == nil && wrapped.Rules != nil {
		rs = wrapped.Rules
	} else if err := json.Unmarshal(b, &rs); err != nil {
		return nil, fmt.Errorf("rules file: want {\"rules\": [...]} or an array: %w", err)
	}
	for i := range rs {
		if !rs[i].Valid() {
			return nil, fmt.Errorf("rule %d: set at least one of module, package, function, tag", i+1)
		}
		if rs[i].ID == "" {
			rs[i].ID = fmt.Sprintf("r%d", i+1)
		}
	}
	return rs, nil
}
