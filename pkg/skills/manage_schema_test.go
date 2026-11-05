package skills

import (
	"encoding/json"
	"sort"
	"testing"
)

// Schema decoded through JSON so every list and object has one shape
func decodedManageSchema(t *testing.T) map[string]any {
	t.Helper()
	data, err := json.Marshal(SkillManageSchema())
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.Unmarshal(data, &out)
	return out
}

func manageSchemaBranches(t *testing.T) []map[string]any {
	params := decodedManageSchema(t)["parameters"].(map[string]any)
	items := params["properties"].(map[string]any)["operations"].(map[string]any)["items"].(map[string]any)
	var out []map[string]any
	for _, b := range items["anyOf"].([]any) {
		out = append(out, b.(map[string]any))
	}
	return out
}

// Reports whether an op satisfies one branch through required keys, known keys and the action enum
func opFitsBranch(op map[string]any, branch map[string]any) bool {
	props := branch["properties"].(map[string]any)
	for _, r := range branch["required"].([]any) {
		if _, ok := op[r.(string)]; !ok {
			return false
		}
	}
	for k := range op {
		if _, ok := props[k]; !ok {
			return false
		}
	}
	for _, a := range props["action"].(map[string]any)["enum"].([]any) {
		if a == op["action"] {
			return true
		}
	}
	return false
}

func matchingBranches(t *testing.T, op map[string]any) int {
	n := 0
	for _, b := range manageSchemaBranches(t) {
		if opFitsBranch(op, b) {
			n++
		}
	}
	return n
}

func TestSkillManageSchemaSingleCallShape(t *testing.T) {
	params := decodedManageSchema(t)["parameters"].(map[string]any)
	var keys []string
	for k := range params["properties"].(map[string]any) {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) != 1 || keys[0] != "operations" {
		t.Fatal(keys)
	}
	if req := params["required"].([]any); len(req) != 1 || req[0] != "operations" {
		t.Fatal(req)
	}
	deleteProps := 0
	for _, b := range manageSchemaBranches(t) {
		req := b["required"].([]any)
		if len(req) < 2 || req[0] != "name" || req[1] != "action" {
			t.Fatal(req)
		}
		props := b["properties"].(map[string]any)
		for _, p := range props {
			if p.(map[string]any)["type"] == "object" {
				t.Fatal("nested object property")
			}
		}
		if enum := props["action"].(map[string]any)["enum"].([]any); len(enum) == 1 && enum[0] == "delete" {
			deleteProps += len(props)
		}
	}
	if deleteProps == 0 {
		t.Fatal("no delete branch")
	}
}

func TestSkillManageSchemaPerActionShapes(t *testing.T) {
	for _, op := range []map[string]any{
		{"name": "s", "action": "create", "file_content": "# x"},
		{"name": "s", "action": "create", "new_string": "# x"},
		{"name": "s", "action": "write_file", "file_path": "references/a.md", "content": "x"},
		{"name": "s", "action": "patch", "file_content": "x"},
	} {
		if n := matchingBranches(t, op); n != 0 {
			t.Fatal(op, n)
		}
	}
	for _, op := range []map[string]any{
		{"name": "s", "action": "create", "content": "# x", "category": "devops"},
		{"name": "s", "action": "patch", "old_string": "a", "new_string": "b", "file_path": "references/a.md", "replace_all": true},
		{"name": "s", "action": "patch", "content": "# whole file"},
		{"name": "s", "action": "write_file", "file_path": "references/a.md", "file_content": "x"},
		{"name": "s", "action": "remove_file", "file_path": "references/a.md"},
		{"name": "s", "action": "delete"},
		{"name": "s", "action": "delete", "absorbed_into": "umbrella"},
	} {
		if n := matchingBranches(t, op); n != 1 {
			t.Fatal(op, n)
		}
	}
}
