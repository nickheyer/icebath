package codeexec

import (
	_ "embed"
	"encoding/json"
)

//go:embed payload/computer_use_schema.json
var computerUseSchemaJSON []byte

var computerUseSchema = decodeComputerUseSchema()

// Frozen schema payload decoded once, a malformed payload is a build defect and panics at init
func decodeComputerUseSchema() map[string]any {
	var out map[string]any
	if err := json.Unmarshal(computerUseSchemaJSON, &out); err != nil {
		panic("computer_use schema payload: " + err.Error())
	}
	return out
}

// Generic function calling schema of the computer_use tool
func GetComputerUseSchema() map[string]any { return computerUseSchema }
