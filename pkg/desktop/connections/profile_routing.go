package connections

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

var profilePathRE = regexp.MustCompile(`^/api/profiles/([^/?#]+)(?:[?#].*)?$`)

// ProfileRequest is the method, path and body of a renderer api request that may rename or delete a profile
type ProfileRequest struct {
	Method any
	Path   any
	Body   any
}

// ProfileRename is the old and new name of a profile rename request
type ProfileRename struct {
	OldName string
	NewName string
}

// Decodes percent escapes the way decodeURIComponent does, refusing malformed escapes and invalid UTF-8
func jsDecodeURIComponent(s string) (string, error) {
	out, err := url.PathUnescape(s)
	if err != nil {
		return "", err
	}
	if !utf8.ValidString(out) {
		return "", errors.New("URI malformed")
	}
	return out, nil
}

// ProfileNameFromPath is the lower cased profile of a /api/profiles/<name> path, empty when it names none
func ProfileNameFromPath(path any) string {
	raw := ""
	if jsTruthy(path) {
		raw = jsString(path)
	}
	match := profilePathRE.FindStringSubmatch(raw)
	if match == nil {
		return ""
	}
	decoded, err := jsDecodeURIComponent(match[1])
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(decoded)
	if name == "" {
		return ""
	}
	return strings.ToLower(name)
}

// Upper cased request method, GET when absent
func requestMethod(method any) string {
	if !jsTruthy(method) {
		return "GET"
	}
	return strings.ToUpper(jsString(method))
}

// ProfileNameFromDeleteRequest is the profile a DELETE /api/profiles/<name> request removes, empty otherwise
func ProfileNameFromDeleteRequest(request *ProfileRequest) string {
	if request == nil || requestMethod(request.Method) != "DELETE" {
		return ""
	}
	return ProfileNameFromPath(request.Path)
}

// Request body as an object, empty for a missing, non object or unparseable body
func parseJSONBody(body any) map[string]any {
	if body == nil || body == "" {
		return map[string]any{}
	}
	if obj, ok := body.(map[string]any); ok {
		return obj
	}
	var parsed any
	if json.Unmarshal([]byte(jsString(body)), &parsed) != nil {
		return map[string]any{}
	}
	if obj, ok := parsed.(map[string]any); ok {
		return obj
	}
	return map[string]any{}
}

// ProfileRenameFromRequest is the rename a PATCH /api/profiles/<name> request with new_name performs, nil otherwise
func ProfileRenameFromRequest(request *ProfileRequest) *ProfileRename {
	if request == nil || requestMethod(request.Method) != "PATCH" {
		return nil
	}
	oldName := ProfileNameFromPath(request.Path)
	if oldName == "" || oldName == "default" {
		return nil
	}
	body := parseJSONBody(request.Body)
	newName := ""
	if jsTruthy(body["new_name"]) {
		newName = jsString(body["new_name"])
	}
	newName = strings.ToLower(strings.TrimSpace(newName))
	if newName == "" || newName == "default" {
		return nil
	}
	return &ProfileRename{OldName: oldName, NewName: newName}
}
