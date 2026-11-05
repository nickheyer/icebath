package provider

import (
	"bytes"
	"fmt"
	"os"
	"unicode/utf8"
)

// Assignments with a value of a utf-8-sig dotenv file interpolated like python-dotenv dotenv_values
func DotenvSetValues(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("'utf-8' codec can't decode %s", path)
	}
	assignments := parseDotenvText(translateUniversalNewlines(string(raw)))
	env := ProcessSourceEnvironment().Snapshot()
	out := map[string]string{}
	for _, a := range resolveDotenvAssignments(assignments, env, true) {
		if a.set {
			out[a.name] = a.value
		}
	}
	return out, nil
}
