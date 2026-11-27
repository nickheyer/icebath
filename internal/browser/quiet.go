package browser

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Loopback port Chromium refuses to dial, so a service endpoint moved there fails without leaving the machine
const deadEndpoint = "http://127.0.0.1:9"

// What keeps the agent's Chromium from calling Google services on its own: first run, default apps, sync, component updates, field
// trials, push messaging, sign in, reporting and new tab doodles
var quietSwitches = []string{
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

// Translate, network time, optimization guide, cast, autofill server, AI mode, search preconnect and the new tab bar and promos
var quietFeatures = []string{
	"Translate", "NetworkTimeServiceQuerying", "OptimizationHints", "OptimizationTargetPrediction",
	"OptimizationGuideOnDeviceModel", "MediaRouter", "AutofillServerCommunication", "AimEnabled",
	"AimServerEligibilityEnabled", "AimServerRequestOnStartupEnabled", "PreconnectToSearch", "NtpOneGoogleBar",
	"NtpMiddleSlotPromo",
}

const (
	disableFeatures = "--disable-features="
	enableFeatures  = "--enable-features="
)

// Switches every agent Chromium starts with, the features as one --disable-features
func QuietArgs() []string {
	return append(slices.Clone(quietSwitches), disableFeatures+strings.Join(quietFeatures, ","))
}

// Argv with every --disable-features and every --enable-features joined into one switch each, at the place of its first
func MergeFeatures(argv []string) []string {
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
			lists[prefix] = nil
		}
		for _, name := range strings.Split(arg[len(prefix):], ",") {
			if name != "" && !slices.Contains(lists[prefix], name) {
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

// Profile files Chromium reads at start with the keys they get, browser sign in left alone for the user's own profile
func quietFiles(owned bool) map[string]map[string]any {
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
		"Default/Preferences": prefs,
		"Local State": {
			"network_time":            map[string]any{"network_time_queries_enabled": false},
			"user_experience_metrics": map[string]any{"reporting_enabled": false},
		},
	}
}

// JSON object of the existing file with the keys set over it at every depth, a missing or unreadable file read as empty
func merge(existing []byte, keys map[string]any) ([]byte, error) {
	doc := map[string]any{}
	if len(bytes.TrimSpace(existing)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(existing))
		decoder.UseNumber()
		var parsed any
		if decoder.Decode(&parsed) == nil {
			if obj, ok := parsed.(map[string]any); ok {
				doc = obj
			}
		}
	}
	setAll(doc, keys)
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(doc); err != nil {
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

// Writes the quiet preferences into the profile files of a user data dir on this machine, through a temp file and a rename; owned for a
// profile of the agent's own, which also has browser sign in off
func Seed(userDataDir string, owned bool) error {
	for rel, keys := range quietFiles(owned) {
		path := filepath.Join(userDataDir, filepath.FromSlash(rel))
		existing, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		data, err := merge(existing, keys)
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

// Runs argv with stdin where the profile lives, answering stdout and the exit code
type Runner func(argv []string, stdin []byte) ([]byte, int, error)

const writeScript = `mkdir -p "$(dirname "$1")" && cat > "$1.icebath-tmp" && mv -f "$1.icebath-tmp" "$1"`

// Writes the quiet preferences into the profile files of a user data dir inside a sandbox or on a remote machine, through run
func SeedRemote(userDataDir string, owned bool, run Runner) error {
	for rel, keys := range quietFiles(owned) {
		path := strings.TrimRight(userDataDir, "/") + "/" + rel
		existing, code, err := run([]string{"bash", "-c", `[ -e "$1" ] || exit 3; cat "$1"`, "icebath-read-pref", path}, nil)
		if err != nil {
			return err
		}
		switch code {
		case 0:
		case 3:
			existing = nil
		default:
			return fmt.Errorf("reading %s exited %d", path, code)
		}
		data, err := merge(existing, keys)
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
