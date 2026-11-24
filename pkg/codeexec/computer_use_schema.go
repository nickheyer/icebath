package codeexec

import (
	_ "embed"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

//go:embed payload/computer_use_schema.json
var computerUseSchemaJSON []byte

var computerUseSchema = decodeComputerUseSchema()

// Frozen schema payload decoded once as json.loads does, a malformed payload is a build defect and panics at init
func decodeComputerUseSchema() *jsonutil.Object {
	out, err := jsonutil.Decode(string(computerUseSchemaJSON))
	if err != nil {
		panic("computer_use schema payload: " + err.Error())
	}
	obj, ok := out.(*jsonutil.Object)
	if !ok {
		panic("computer_use schema payload is not a JSON object")
	}
	return obj
}

// Generic function calling schema of the computer_use tool
func GetComputerUseSchema() *jsonutil.Object { return computerUseSchema }
