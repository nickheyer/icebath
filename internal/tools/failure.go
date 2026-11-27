package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
)

// Key a refusal of a redundant call carries, which no failure is
const GuardrailRefusal = "guardrail_refusal"

var (
	failureMarker = regexp.MustCompile(`(?i)"(?:error|failed)"`)
	emptyValue    = regexp.MustCompile(`^\s*:\s*(?:null|false|0|""|\[\]|\{\})\s*(?:[,}\]]|$)`)
)

// Result of a tool read as a JSON object, nil when it is none
func Object(result string) map[string]any {
	var object map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(result)), &object) != nil {
		return nil
	}
	return object
}

// Whether a result says the call failed, and in a word how: a command's non zero exit, an error the result names, a failed status, every
// result of a batch failing, else an error marker near its start. A write that landed and a refusal of a redundant call are no failures
func Broke(tool, result string) (bool, string) {
	if Wrote(tool, result) {
		return false, ""
	}
	object := Object(result)
	if object != nil && object[GuardrailRefusal] == true {
		return false, ""
	}
	if tool == "terminal" {
		code, ok := object["exit_code"]
		if !ok || code == nil {
			return false, ""
		}
		if n, number := code.(float64); number && n == float64(int64(n)) {
			return n != 0, fmt.Sprintf("exit %d", int64(n))
		}
		return true, fmt.Sprintf("exit %v", code)
	}
	if object != nil {
		success, typed := object["success"].(bool)
		value := object["error"]
		if !config.Truthy(value) {
			value = object["message"]
		}
		_, named := object["error"]
		return config.Truthy(value) && (typed && !success || named) || shapeFailure(object), "error"
	}
	head := result[:min(len(result), 600)]
	for _, at := range failureMarker.FindAllStringIndex(head, -1) {
		if at[0] < 500 && !emptyValue.MatchString(head[at[1]:]) {
			return true, "error"
		}
	}
	return strings.HasPrefix(result, "Error"), "error"
}

// Whether an object result failed by its shape: a failed status, or every one of its results an error
func shapeFailure(object map[string]any) bool {
	if status, ok := object["status"]; ok && status != nil {
		if s := strings.ToLower(fmt.Sprint(status)); s == "failed" || s == "error" {
			return true
		}
	}
	items, ok := object["results"].([]any)
	if !ok || len(items) == 0 {
		return false
	}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok || !config.Truthy(entry["error"]) {
			return false
		}
	}
	return true
}

// Whether a file write's result proves it landed
func Wrote(tool, result string) bool {
	object := Object(result)
	if object == nil || config.Truthy(object["error"]) {
		return false
	}
	switch tool {
	case "write_file":
		_, ok := object["bytes_written"]
		return ok
	case "patch":
		return object["success"] == true
	}
	return false
}

// Error a result names, as text: a text as it is, any other value as the JSON it was written as; empty when it names none
func ErrorText(result string) string {
	var body map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(result)), &body) != nil {
		return ""
	}
	raw, ok := body["error"]
	if !ok {
		return ""
	}
	var value any
	if json.Unmarshal(raw, &value) != nil || !config.Truthy(value) {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return string(raw)
}
