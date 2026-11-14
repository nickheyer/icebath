package aws

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/home"
)

type section = map[string]string

type sharedFiles struct {
	profiles    map[string]section
	config      map[string]section
	credentials map[string]section
	ssoSessions map[string]section
	services    map[string]map[string]section
}

type iniDoc struct {
	order  []string
	flat   map[string]section
	nested map[string]map[string]section
}

var iniOption = regexp.MustCompile(`^(.*?)\s*([=:])\s*(.*)$`)

func expandPath(path string) string {
	path = expandVars(path)
	return home.ExpandUser(path)
}

func expandVars(path string) string {
	path = expandPlatformVars(path)
	if !strings.Contains(path, "$") {
		return path
	}
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] != '$' || i+1 >= len(path) {
			b.WriteByte(path[i])
			continue
		}
		rest := path[i+1:]
		name, width := "", 0
		if rest[0] == '{' {
			if end := strings.IndexByte(rest, '}'); end > 0 {
				name, width = rest[1:end], end+1
			}
		} else {
			n := 0
			for n < len(rest) && (rest[n] == '_' || rest[n] >= '0' && rest[n] <= '9' || rest[n] >= 'a' && rest[n] <= 'z' || rest[n] >= 'A' && rest[n] <= 'Z') {
				n++
			}
			name, width = rest[:n], n
		}
		if value, ok := os.LookupEnv(name); ok && name != "" {
			b.WriteString(value)
			i += width
			continue
		}
		b.WriteByte(path[i])
	}
	return b.String()
}

func sharedPath(envName, name string) string {
	if v, ok := os.LookupEnv(envName); ok {
		return expandPath(v)
	}
	return expandPath("~/.aws/" + name)
}

func awsDir(parts ...string) string {
	return expandPath("~/.aws/" + strings.Join(parts, "/"))
}

func readINI(path string) (*iniDoc, bool, error) {
	if path == "" {
		return nil, false, nil
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, nil
	}
	if info.Size() > 16<<20 {
		return nil, false, &Error{Source: "config", Message: "config file " + path + " is larger than 16 MiB"}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, &Error{Source: "config", Message: "unable to read config file " + path, Err: err}
	}
	doc, err := parseINI(string(data))
	if err != nil {
		return nil, false, &Error{Source: "config", Message: "unable to parse config file " + path + ": " + err.Error()}
	}
	return doc, true, nil
}

func parseINI(text string) (*iniDoc, error) {
	type raw struct{ values map[string][]string }
	sections := map[string]*raw{}
	var order []string
	defaults := &raw{values: map[string][]string{}}
	var cur *raw
	curOpt := ""
	indent := 0
	var bad []string
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	for lineno, line := range strings.Split(text, "\n") {
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "#") || strings.HasPrefix(stripped, ";") {
			continue
		}
		if stripped == "" {
			if cur != nil && curOpt != "" {
				cur.values[curOpt] = append(cur.values[curOpt], "")
			}
			continue
		}
		lead := len(line) - len(strings.TrimLeft(line, " \t\f\v"))
		if cur != nil && curOpt != "" && lead > indent {
			cur.values[curOpt] = append(cur.values[curOpt], stripped)
			continue
		}
		indent = lead
		if strings.HasPrefix(stripped, "[") {
			if end := strings.LastIndex(stripped, "]"); end > 1 {
				name := stripped[1:end]
				curOpt = ""
				if name == "DEFAULT" {
					cur = defaults
					continue
				}
				if _, dup := sections[name]; dup {
					return nil, fmt.Errorf("section %q appears twice", name)
				}
				cur = &raw{values: map[string][]string{}}
				sections[name] = cur
				order = append(order, name)
				continue
			}
		}
		if cur == nil {
			return nil, fmt.Errorf("line %d comes before any section header", lineno+1)
		}
		m := iniOption.FindStringSubmatch(stripped)
		if m == nil || strings.TrimSpace(m[1]) == "" {
			bad = append(bad, fmt.Sprintf("line %d", lineno+1))
			curOpt = ""
			continue
		}
		opt := strings.ToLower(strings.TrimRight(m[1], " \t"))
		if _, dup := cur.values[opt]; dup {
			return nil, fmt.Errorf("option %q appears twice in one section", opt)
		}
		cur.values[opt] = []string{strings.TrimSpace(m[3])}
		curOpt = opt
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("unreadable %s", strings.Join(bad, ", "))
	}
	doc := &iniDoc{order: order, flat: map[string]section{}, nested: map[string]map[string]section{}}
	for _, name := range order {
		flat := section{}
		nested := map[string]section{}
		merged := map[string][]string{}
		for k, v := range defaults.values {
			merged[k] = v
		}
		for k, v := range sections[name].values {
			merged[k] = v
		}
		for k, v := range merged {
			value := strings.TrimRight(strings.Join(v, "\n"), " \t\n\r\f\v")
			if strings.HasPrefix(value, "\n") {
				sub, err := parseNested(value)
				if err != nil {
					return nil, err
				}
				nested[k] = sub
			}
			flat[k] = value
		}
		doc.flat[name] = flat
		doc.nested[name] = nested
	}
	return doc, nil
}

func parseNested(value string) (section, error) {
	out := section{}
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("nested line %q has no value", line)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

func sectionName(header string) (string, bool) {
	parts, err := posixSplit(header)
	if err != nil || len(parts) != 2 {
		return "", false
	}
	return parts[1], true
}

func loadShared() (*sharedFiles, error) {
	out := &sharedFiles{
		profiles: map[string]section{}, config: map[string]section{}, credentials: map[string]section{},
		ssoSessions: map[string]section{}, services: map[string]map[string]section{},
	}
	cfg, found, err := readINI(sharedPath("AWS_CONFIG_FILE", "config"))
	if err != nil {
		return nil, err
	}
	if found {
		for _, sec := range cfg.order {
			values := cfg.flat[sec]
			switch {
			case strings.HasPrefix(sec, "profile"):
				if name, ok := sectionName(sec); ok {
					out.config[name] = values
				}
			case strings.HasPrefix(sec, "sso-session"):
				if name, ok := sectionName(sec); ok {
					out.ssoSessions[name] = values
				}
			case strings.HasPrefix(sec, "services"):
				if name, ok := sectionName(sec); ok {
					out.services[name] = cfg.nested[sec]
				}
			case sec == "default":
				out.config["default"] = values
			}
		}
	}
	creds, found, err := readINI(sharedPath("AWS_SHARED_CREDENTIALS_FILE", "credentials"))
	if err != nil {
		return nil, err
	}
	if found {
		for _, sec := range creds.order {
			out.credentials[sec] = creds.flat[sec]
		}
	}
	for name, p := range out.config {
		merged := make(section, len(p))
		for k, v := range p {
			merged[k] = v
		}
		out.profiles[name] = merged
	}
	for name, p := range out.credentials {
		merged, ok := out.profiles[name]
		if !ok {
			merged = section{}
			out.profiles[name] = merged
		}
		for k, v := range p {
			merged[k] = v
		}
	}
	return out, nil
}

type session struct {
	files   *sharedFiles
	profile string
	named   bool
}

func newSession(profile string) (*session, error) {
	files, err := loadShared()
	if err != nil {
		return nil, err
	}
	s := &session{files: files}
	if profile != "" {
		s.profile, s.named = profile, true
		return s, nil
	}
	for _, name := range []string{"AWS_DEFAULT_PROFILE", "AWS_PROFILE"} {
		if v, ok := os.LookupEnv(name); ok {
			s.profile, s.named = v, true
			break
		}
	}
	return s, nil
}

func (s *session) name() string {
	if s.profile == "" {
		return "default"
	}
	return s.profile
}

func (s *session) scoped() (section, error) {
	if !s.named {
		return s.files.profiles["default"], nil
	}
	p, ok := s.files.profiles[s.profile]
	if !ok {
		return nil, &Error{Source: "config", Message: "the config profile (" + s.profile + ") could not be found"}
	}
	return p, nil
}

func (s *session) setting(envName, key string) (string, bool) {
	if envName != "" {
		if v, ok := os.LookupEnv(envName); ok {
			return v, true
		}
	}
	p, err := s.scoped()
	if err != nil {
		return "", false
	}
	v, ok := p[key]
	return v, ok
}

func (s *session) value(envName, key string) string {
	v, _ := s.setting(envName, key)
	return v
}

func (s *session) region() string {
	return s.value("AWS_DEFAULT_REGION", "region")
}

func isTrue(v string) bool { return strings.EqualFold(strings.TrimSpace(v), "true") }

func caBundle(profile string) string {
	if v := os.Getenv("AWS_CA_BUNDLE"); v != "" {
		return expandPath(v)
	}
	s, err := newSession(profile)
	if err != nil {
		return ""
	}
	if v := s.value("", "ca_bundle"); v != "" {
		return expandPath(v)
	}
	return ""
}

func (s *session) configuredEndpoint(serviceID string) string {
	snake := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(serviceID))
	if v, ok := s.setting("AWS_IGNORE_CONFIGURED_ENDPOINT_URLS", "ignore_configured_endpoint_urls"); ok && isTrue(v) {
		return ""
	}
	if v := os.Getenv("AWS_ENDPOINT_URL_" + strings.ToUpper(snake)); v != "" {
		return v
	}
	if v := os.Getenv("AWS_ENDPOINT_URL"); v != "" {
		return v
	}
	p, err := s.scoped()
	if err != nil {
		return ""
	}
	if name := p["services"]; name != "" {
		if v := s.files.services[name][snake]["endpoint_url"]; v != "" {
			return v
		}
	}
	return p["endpoint_url"]
}

type partition struct {
	name       string
	dnsSuffix  string
	dualSuffix string
}

func partitionOf(region string) partition {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return partition{"aws-cn", "amazonaws.com.cn", "api.amazonwebservices.com.cn"}
	case strings.HasPrefix(region, "us-gov-"):
		return partition{"aws-us-gov", "amazonaws.com", "api.aws"}
	case strings.HasPrefix(region, "us-isob-"):
		return partition{"aws-iso-b", "sc2s.sgov.gov", ""}
	case strings.HasPrefix(region, "us-isof-"):
		return partition{"aws-iso-f", "csp.hci.ic.gov", ""}
	case strings.HasPrefix(region, "us-iso-"):
		return partition{"aws-iso", "c2s.ic.gov", ""}
	case strings.HasPrefix(region, "eu-isoe-"):
		return partition{"aws-iso-e", "cloud.adc-e.uk", ""}
	case strings.HasPrefix(region, "eusc-"):
		return partition{"aws-eusc", "amazonaws.eu", ""}
	}
	return partition{"aws", "amazonaws.com", "api.aws"}
}

func (s *session) endpoint(serviceID, hostPrefix, region string) string {
	if ep := s.configuredEndpoint(serviceID); ep != "" {
		return strings.TrimRight(ep, "/")
	}
	p := partitionOf(region)
	fips := isTrue(s.value("AWS_USE_FIPS_ENDPOINT", "use_fips_endpoint"))
	dual := isTrue(s.value("AWS_USE_DUALSTACK_ENDPOINT", "use_dualstack_endpoint")) && p.dualSuffix != ""
	govPlain := p.name == "aws-us-gov" && (hostPrefix == "sts" || hostPrefix == "portal.sso" || hostPrefix == "oidc")
	suffix := p.dnsSuffix
	if dual {
		suffix = p.dualSuffix
	}
	if fips && !govPlain {
		hostPrefix += "-fips"
	}
	return "https://" + hostPrefix + "." + region + "." + suffix
}

func parseTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if strings.HasSuffix(v, "UTC") {
		v = strings.TrimSpace(strings.TrimSuffix(v, "UTC")) + "Z"
	}
	layouts := []string{
		time.RFC3339Nano, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05.999999999Z0700", "2006-01-02T15:04:05.999999999",
		"2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999Z0700", "2006-01-02 15:04:05.999999999", "20060102T150405Z",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("timestamp %q is not in a known layout", v)
}

func cacheFile(dir, name string) string { return filepath.Join(dir, name+".json") }
