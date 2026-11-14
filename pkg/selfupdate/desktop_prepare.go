package selfupdate

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Environment names a desktop bundle may carry as non secret defaults and explicit clears
var bundleEnvAllowed = map[string]bool{
	"ICEBATH_HOME": true, "ICEBATH_DATA_DIR_SUFFIX": true, "ICEBATH_DESKTOP_USER_DATA_DIR": true,
	"ICEBATH_SHARED_AUTH_DIR": true, "ICEBATH_PREVIEW_FULL_CONNECTORS": true,
}

// Rejects a bundle environment naming a variable outside the allowed set or holding a non string value
func ValidateBundleEnv(values any) (*pyjson.Object, error) {
	obj, ok := values.(*pyjson.Object)
	if !ok {
		return nil, &ValueError{Msg: "Bundle environment must be a JSON object"}
	}
	for _, key := range obj.Keys {
		if !bundleEnvAllowed[key] {
			return nil, &ValueError{Msg: "Bundle environment name is not permitted: " + key}
		}
		value := obj.Vals[key]
		if s, isStr := value.(string); value != nil && (!isStr || strings.Contains(s, "\x00")) {
			return nil, &ValueError{Msg: fmt.Sprintf("Bundle environment value for %s must be a string without NUL or null", key)}
		}
	}
	return obj, nil
}

var reservedWindowsName = regexp.MustCompile(`^(?i:con|prn|aux|nul|com[0-9]|lpt[0-9])$`)

// Admits a channel request under the shared release protocol plus the native packaging constraints
func ValidateDesktopChannelRequest(body []byte) (*pyjson.Object, error) {
	value, err := decodeChannelJSON(body)
	if err != nil {
		return nil, err
	}
	if _, err := validateChannelRequest(value, nil, nil, "preview"); err != nil {
		return nil, err
	}
	if _, err := ValidateBundleEnv(value.Vals["bundleEnv"]); err != nil {
		return nil, err
	}
	identity, _ := value.Vals["identity"].(*pyjson.Object)
	for _, field := range []string{"cliName", "windowsExecutableName", "appNamePascal", "artifactNamePascal"} {
		name, _ := identity.Vals[field].(string)
		if reservedWindowsName.MatchString(strings.Split(name, ".")[0]) {
			return nil, &ValueError{Msg: "reserved channel native identity " + field}
		}
	}
	if exe, _ := identity.Vals["windowsExecutableName"].(string); strings.HasSuffix(exe, ".") || strings.HasSuffix(exe, " ") {
		return nil, &ValueError{Msg: "invalid channel native identity windowsExecutableName"}
	}
	if base, _ := value.Vals["publicBase"].(string); !strings.HasPrefix(base, "https://") {
		return nil, &ValueError{Msg: "channel publicBase must be HTTPS for packaged clients"}
	}
	return pyjson.Clone(value).(*pyjson.Object), nil
}
