// Package quiet keeps the Chromium icebath starts for the agent from calling Google services on its own
package quiet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Loopback address on a port Chromium refuses to dial, so a service endpoint moved here fails without leaving the machine
const deadEndpoint = "http://127.0.0.1:9"

// First run, default apps, sync, component updates, field trials, push messaging, sign in, reporting and new tab doodles
var switches = []string{
	"--no-first-run", "--no-default-browser-check", "--disable-search-engine-choice-screen",
	"--disable-background-networking", "--disable-component-update", "--disable-default-apps",
	"--disable-component-extensions-with-background-pages", "--disable-sync",
	"--disable-breakpad", "--metrics-recording-only", "--disable-domain-reliability", "--disable-field-trial-config",
	"--variations-server-url=" + deadEndpoint,
	"--component-updater=url-source=" + deadEndpoint + ",disable-pings",
	"--gcm-checkin-url=" + deadEndpoint, "--gcm-registration-url=" + deadEndpoint, "--gcm-mcs-endpoint=" + deadEndpoint,
	"--gaia-url=" + deadEndpoint, "--google-apis-url=" + deadEndpoint,
	"--google-doodle-url=" + deadEndpoint, "--third-party-doodle-url=" + deadEndpoint,
}

// Translate, network time, optimization guide, cast, autofill server, AI mode, search preconnect and new tab bar and promos
var disabledFeatures = []string{
	"Translate", "NetworkTimeServiceQuerying", "OptimizationHints", "OptimizationTargetPrediction",
	"OptimizationGuideOnDeviceModel", "MediaRouter", "AutofillServerCommunication", "AimEnabled",
	"AimServerEligibilityEnabled", "AimServerRequestOnStartupEnabled", "PreconnectToSearch", "NtpOneGoogleBar",
	"NtpMiddleSlotPromo",
}

const (
	disableFeatures = "--disable-features="
	enableFeatures  = "--enable-features="
)

// Switches every agent Chromium starts with, the feature list as one --disable-features
func Args() []string {
	out := append([]string{}, switches...)
	return append(out, disableFeatures+strings.Join(disabledFeatures, ","))
}

// Argv with every --disable-features and every --enable-features joined into one switch each at its first place
func MergeFeatureSwitches(argv []string) []string {
	lists := map[string][]string{}
	out := make([]string, 0, len(argv))
	for _, arg := range argv {
		prefix := ""
		for _, p := range []string{disableFeatures, enableFeatures} {
			if strings.HasPrefix(arg, p) {
				prefix = p
			}
		}
		if prefix == "" {
			out = append(out, arg)
			continue
		}
		if _, seen := lists[prefix]; !seen {
			out = append(out, prefix)
		}
		for _, name := range strings.Split(arg[len(prefix):], ",") {
			if name != "" && !contains(lists[prefix], name) {
				lists[prefix] = append(lists[prefix], name)
			}
		}
	}
	for i, arg := range out {
		if names, ok := lists[arg]; ok {
			out[i] = arg + strings.Join(names, ",")
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Profile files Chromium reads at start with the keys they get, browser sign in kept for the user's own profile
func Files(owned bool) map[string]map[string]any {
	prefs := map[string]any{
		"search":                map[string]any{"suggest_enabled": false},
		"net":                   map[string]any{"network_prediction_options": 2},
		"alternate_error_pages": map[string]any{"enabled": false},
		"safebrowsing":          map[string]any{"enabled": false, "enhanced": false},
		"browser":               map[string]any{"enable_spellchecking": false},
		"spellcheck":            map[string]any{"use_spelling_service": false},
		"translate":             map[string]any{"enabled": false},
		"profile":               map[string]any{"password_manager_leak_detection": false},
	}
	if owned {
		prefs["signin"] = map[string]any{"allowed": false, "allowed_on_next_startup": false}
	}
	return map[string]map[string]any{
		filepath.ToSlash(filepath.Join("Default", "Preferences")): prefs,
		"Local State": {
			"network_time":            map[string]any{"network_time_queries_enabled": false},
			"user_experience_metrics": map[string]any{"reporting_enabled": false},
		},
	}
}

// JSON object of existing with keys set over it at every depth, a missing or unreadable file read as empty
func Merge(existing []byte, keys map[string]any) ([]byte, error) {
	doc := map[string]any{}
	if len(bytes.TrimSpace(existing)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(existing))
		dec.UseNumber()
		var parsed any
		if err := dec.Decode(&parsed); err == nil {
			if obj, ok := parsed.(map[string]any); ok {
				doc = obj
			}
		}
	}
	setAll(doc, keys)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func setAll(dst, src map[string]any) {
	for k, v := range src {
		if sub, ok := v.(map[string]any); ok {
			child, ok := dst[k].(map[string]any)
			if !ok {
				child = map[string]any{}
				dst[k] = child
			}
			setAll(child, sub)
			continue
		}
		dst[k] = v
	}
}

// Writes the keys into the profile files of a local user data dir through a temp file and rename
func SeedProfile(userDataDir string, owned bool) error {
	for rel, keys := range Files(owned) {
		path := filepath.Join(userDataDir, filepath.FromSlash(rel))
		existing, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		data, err := Merge(existing, keys)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		tmp := path + ".icebath-tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return nil
}

// Runs argv with stdin where the profile lives, returning stdout and the exit code
type Runner func(argv []string, stdin []byte) ([]byte, int, error)

const writeScript = `mkdir -p "$(dirname "$1")" && cat > "$1.icebath-tmp" && mv -f "$1.icebath-tmp" "$1"`

// Writes the keys into the profile files of a user data dir inside a container or remote host through run
func SeedRemoteProfile(userDataDir string, owned bool, run Runner) error {
	for rel, keys := range Files(owned) {
		path := strings.TrimRight(userDataDir, "/") + "/" + rel
		existing, code, err := run([]string{"bash", "-c", `[ -e "$1" ] || exit 3; cat "$1"`, "icebath-read-pref", path}, nil)
		if err != nil {
			return err
		}
		if code == 3 {
			existing = nil
		} else if code != 0 {
			return fmt.Errorf("reading %s exited %d", path, code)
		}
		data, err := Merge(existing, keys)
		if err != nil {
			return err
		}
		if _, code, err := run([]string{"bash", "-c", writeScript, "icebath-write-pref", path}, data); err != nil {
			return err
		} else if code != 0 {
			return fmt.Errorf("writing %s exited %d", path, code)
		}
	}
	return nil
}
