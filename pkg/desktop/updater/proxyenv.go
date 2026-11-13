package updater

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/desktop/sysproxy"
)

const (
	updateProxyMarker = "ICEBATH_UPDATE_SYSTEM_PROXY"
	savedProxyPrefix  = "ICEBATH_UPDATE_USER_"
)

var (
	proxyEnvKeys  = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"}
	proxySetKeys  = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"}
	noProxyKeys   = []string{"NO_PROXY", "no_proxy"}
	updateHosts   = []string{"github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com", "hermes-assets.nousresearch.com", "registry.npmjs.org", "pypi.org", "files.pythonhosted.org", "releases.astral.sh"}
	localBypass   = []string{"localhost", "127.0.0.1", "::1", "127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", ".local"}
	indexEnvKeys  = []string{"uv_index_url", "uv_extra_index_url", "uv_default_index", "uv_index", "pip_index_url", "pip_extra_index_url", "npm_config_registry"}
	uvIndexKeyRE  = regexp.MustCompile(`^uv_index_.+_url$`)
	iniSectionRE  = regexp.MustCompile(`^\[(.+)\]$`)
	httpSchemeRE  = regexp.MustCompile(`(?i)^https?:`)
	resolveSystem = sysproxy.Resolve
)

// Kind and host port of the first entry of a PAC answer
func pacKind(answer string) (string, string) {
	first, _, _ := strings.Cut(answer, ";")
	fields := strings.Fields(strings.TrimSpace(first))
	kind, hostPort := "", ""
	if len(fields) > 0 {
		kind = fields[0]
	}
	if len(fields) > 1 {
		hostPort = fields[1]
	}
	return strings.ToUpper(kind), hostPort
}

// HTTP proxy URL of a plain PROXY answer, empty for every other kind
func proxyURLFromPAC(answer string) string {
	kind, hostPort := pacKind(answer)
	if kind == "PROXY" && hostPort != "" {
		return "http://" + hostPort
	}
	return ""
}

// The user's own proxy variables, the ones a previous update added removed and saved no proxy values put back
func restoreUserProxyEnv(env map[string]string) map[string]string {
	for _, key := range strings.Split(env[updateProxyMarker], ",") {
		if slices.Contains(proxySetKeys, key) || slices.Contains(noProxyKeys, key) {
			delete(env, key)
		}
	}
	for _, key := range noProxyKeys {
		if saved, ok := env[savedProxyPrefix+key]; ok {
			env[key] = saved
			delete(env, savedProxyPrefix+key)
		}
	}
	delete(env, updateProxyMarker)
	return env
}

func valuesAnyCase(env map[string]string, name string) []string {
	var out []string
	for key, value := range env {
		if strings.ToLower(key) == name {
			out = append(out, value)
		}
	}
	return out
}

// No proxy variables keeping the user's entries, local ranges and hosts the system resolves direct
func noProxyEnv(userEnv map[string]string, direct []string, isWindows bool) (map[string]string, []string) {
	var entries []string
	for _, entry := range strings.Split(strings.Join(valuesAnyCase(userEnv, "no_proxy"), ","), ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			entries = append(entries, entry)
		}
	}
	var unique []string
	for _, entry := range append(append(entries, localBypass...), direct...) {
		if !slices.Contains(unique, entry) {
			unique = append(unique, entry)
		}
	}
	value := strings.Join(unique, ",")
	env := map[string]string{}
	var added []string
	for _, key := range noProxyKeys {
		env[key] = value
		own, ok := userEnv[key]
		if !ok && isWindows {
			if values := valuesAnyCase(userEnv, "no_proxy"); len(values) > 0 {
				own, ok = values[0], true
			}
		}
		if !ok {
			added = append(added, key)
		} else {
			env[savedProxyPrefix+key] = own
		}
	}
	return env, added
}

// System proxy as update environment variables, nothing when the user set proxies or no single plain proxy answers
func systemProxyEnv(env map[string]string, resolve func(string) (string, error), hosts []string, note func(string), isWindows bool) map[string]string {
	copied := make(map[string]string, len(env))
	for key, value := range env {
		copied[key] = value
	}
	userEnv := restoreUserProxyEnv(copied)
	for _, key := range proxyEnvKeys {
		for _, value := range valuesAnyCase(userEnv, strings.ToLower(key)) {
			if strings.TrimSpace(value) != "" {
				return map[string]string{}
			}
		}
	}
	for _, value := range valuesAnyCase(userEnv, "no_proxy") {
		if slices.Contains(strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' }), "*") {
			return map[string]string{}
		}
	}
	type answer struct {
		host, kind, proxy string
	}
	answers := make([]answer, len(hosts))
	var wg sync.WaitGroup
	for i, host := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, err := resolve("https://" + host + "/")
			if err != nil {
				raw = "DIRECT"
			}
			kind, _ := pacKind(raw)
			if kind == "" {
				kind = "DIRECT"
			}
			answers[i] = answer{host: host, kind: kind, proxy: proxyURLFromPAC(raw)}
		}()
	}
	wg.Wait()
	var routed []answer
	var direct []string
	proxies := map[string]bool{}
	for _, a := range answers {
		if a.kind == "DIRECT" {
			direct = append(direct, a.host)
			continue
		}
		routed = append(routed, a)
		proxies[a.proxy] = true
	}
	if len(routed) == 0 {
		return map[string]string{}
	}
	if len(proxies) > 1 || proxies[""] {
		parts := make([]string, len(routed))
		for i, a := range routed {
			parts[i] = a.host + "=" + a.kind
		}
		if note != nil {
			note("[updates] system proxy not passed to the update: " + strings.Join(parts, ", ") + " (only one plain PROXY answer can be)")
		}
		return map[string]string{}
	}
	noProxy, added := noProxyEnv(userEnv, direct, isWindows)
	out := map[string]string{}
	for _, key := range proxySetKeys {
		out[key] = routed[0].proxy
	}
	for key, value := range noProxy {
		out[key] = value
	}
	out[updateProxyMarker] = strings.Join(append(append([]string(nil), proxySetKeys...), added...), ",")
	return out
}

// Update proxy variables for an update of updateRoot, its configured package hosts resolved beside the fixed ones
func updateProxyEnv(env map[string]string, note func(string), updateRoot string) map[string]string {
	hosts := append([]string(nil), updateHosts...)
	if updateRoot != "" {
		userHome, _ := os.UserHomeDir()
		for _, host := range configuredUpdateHosts(updateRoot, env, userHome) {
			if !slices.Contains(hosts, host) {
				hosts = append(hosts, host)
			}
		}
	}
	return systemProxyEnv(env, resolveSystem, hosts, note, runtime.GOOS == "windows")
}

// Lower cased host of an http or https URL, a key=URL form read past its key
func httpHost(value string) string {
	raw := value
	if strings.Contains(value, "=") && !httpSchemeRE.MatchString(value) {
		raw = value[strings.Index(value, "=")+1:]
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

func readText(file string) string {
	raw, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	return string(raw)
}

// Values of keys inside one ini section, quotes stripped
func iniValues(text, section string, keys []string) []string {
	current := ""
	var values []string
	for _, raw := range regexp.MustCompile(`\r?\n`).Split(text, -1) {
		line := strings.TrimSpace(raw)
		if header := iniSectionRE.FindStringSubmatch(line); header != nil {
			current = strings.ToLower(strings.TrimSpace(header[1]))
			continue
		}
		if current != section {
			continue
		}
		parts := strings.Split(line, "=")
		if len(parts) > 1 && slices.Contains(keys, strings.ToLower(strings.TrimSpace(parts[0]))) {
			value := strings.TrimSpace(strings.Join(parts[1:], "="))
			if strings.HasPrefix(value, `"`) || strings.HasPrefix(value, "'") {
				value = value[1:]
			}
			if strings.HasSuffix(value, `"`) || strings.HasSuffix(value, "'") {
				value = value[:len(value)-1]
			}
			values = append(values, value)
		}
	}
	return values
}

func pipConfigFiles(env map[string]string, home string) []string {
	var files []string
	if runtime.GOOS == "windows" {
		appData := env["APPDATA"]
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		files = []string{filepath.Join(home, "pip", "pip.ini"), filepath.Join(appData, "pip", "pip.ini")}
	} else {
		xdg := env["XDG_CONFIG_HOME"]
		if xdg == "" {
			xdg = filepath.Join(home, ".config")
		}
		files = []string{filepath.Join(home, ".pip", "pip.conf"), filepath.Join(home, "Library", "Application Support", "pip", "pip.conf"), filepath.Join(xdg, "pip", "pip.conf")}
	}
	if explicit := env["PIP_CONFIG_FILE"]; explicit != "" {
		files = append(files, explicit)
	}
	return files
}

// Hosts an update of updateRoot reaches beyond the fixed list, its git remote and package indexes
func configuredUpdateHosts(updateRoot string, env map[string]string, home string) []string {
	values := iniValues(readText(filepath.Join(updateRoot, ".git", "config")), `remote "origin"`, []string{"url"})
	for key, value := range env {
		name := strings.ToLower(key)
		if value != "" && (slices.Contains(indexEnvKeys, name) || uvIndexKeyRE.MatchString(name)) {
			values = append(values, strings.Fields(value)...)
		}
	}
	npmrc := env["npm_config_userconfig"]
	if npmrc == "" {
		npmrc = env["NPM_CONFIG_USERCONFIG"]
	}
	if npmrc == "" {
		npmrc = filepath.Join(home, ".npmrc")
	}
	values = append(values, iniValues(readText(npmrc), "", []string{"registry"})...)
	for _, file := range pipConfigFiles(env, home) {
		for _, value := range iniValues(readText(file), "global", []string{"index-url", "extra-index-url"}) {
			values = append(values, strings.Fields(value)...)
		}
	}
	var hosts []string
	for _, value := range values {
		if host := httpHost(value); host != "" && !slices.Contains(hosts, host) {
			hosts = append(hosts, host)
		}
	}
	return hosts
}
