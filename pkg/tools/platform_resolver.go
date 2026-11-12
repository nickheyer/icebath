package tools

import (
	"bytes"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyplist"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// ResolverEffort is what a probe may do, network never implying a subprocess
type ResolverEffort string

const (
	EffortLocal      ResolverEffort = "local"
	EffortNetwork    ResolverEffort = "network"
	EffortDiagnostic ResolverEffort = "diagnostic"
)

// resolverInspection is the in process inspect tier result
type resolverInspection struct {
	Version observation[string]
	Signer  observation[string]
}

// True unless the version or signer read errored
func (i resolverInspection) OK() bool {
	return i.Version.State != checkError && i.Signer.State != checkError
}

// resolverProbe is the fresh probe tier result, a struct that never reads as a boolean
type resolverProbe struct {
	Running   observation[bool]
	Answering observation[bool]
	Endpoint  observation[string]
}

type platformResolver interface {
	Name() string
	Locate(ctx *commandLookupContext) commandResolution
	Inspect(res commandResolution, ctx *commandLookupContext) resolverInspection
}

type platformProbeable interface {
	platformResolver
	Probe(res commandResolution, effort ResolverEffort, deadline float64) (resolverProbe, error)
}

var _ platformProbeable = PlatformAppResolver{}

var platformLoopbackHosts = []string{"127.0.0.1", "localhost", "::1", "[::1]"}

// PlatformAppLocation is one place to look, a path or a location kind value with an install relative file
type PlatformAppLocation struct {
	Kind  string
	Value string
	File  string
}

// PlatformAppDef is one application on one OS, paths expanded at lookup
type PlatformAppDef struct {
	AppID            string
	OSFamily         string
	Presence         string
	Locations        []PlatformAppLocation
	VersionKind      string
	VersionArg       string
	LivenessKind     string
	LivenessPath     string
	LivenessPIDKey   string
	LivenessURLKey   string
	LivenessTokenKey string
	EndpointPath     string
}

// Definition with the dataclass defaults for every optional field
func NewPlatformAppDef(appID, osFamily, presence string, locations ...PlatformAppLocation) PlatformAppDef {
	return PlatformAppDef{AppID: appID, OSFamily: osFamily, Presence: presence, Locations: locations,
		VersionKind: "none", LivenessKind: "none", LivenessPIDKey: "pid", LivenessURLKey: "http",
		LivenessTokenKey: "token", EndpointPath: "/mcp"}
}

func platformExpand(path string) string { return osPathExpandVars(osPathExpandUser(path)) }

// PlatformEndpoint is a validated runtime endpoint whose token never prints
type PlatformEndpoint struct {
	URL   string
	Token string
}

func (e PlatformEndpoint) String() string {
	return fmt.Sprintf("Endpoint(url=%s, token=<redacted>)", pyrepr.StrRepr(e.URL))
}

func (e PlatformEndpoint) GoString() string { return e.String() }

// PlatformAppResolver resolves a desktop application declared by a PlatformAppDef
type PlatformAppResolver struct{ Definition PlatformAppDef }

func (r PlatformAppResolver) Name() string { return r.Definition.AppID }

// Stats every declared location in order, the first present hit setting the resolution kind
func (r PlatformAppResolver) Locate(ctx *commandLookupContext) commandResolution {
	d := r.Definition
	var found []platformHit
	for _, loc := range d.Locations {
		found = append(found, platformLocator(loc.Kind)(d, loc, ctx)...)
	}
	res := commandResolution{Kind: "missing"}
	for _, h := range found {
		if h.Candidate.Present {
			res.Kind = h.Kind
			break
		}
	}
	for _, h := range found {
		res.Candidates = append(res.Candidates, h.Candidate)
	}
	return res
}

// Reads the version source of the first present copy, not checked when missing
func (r PlatformAppResolver) Inspect(res commandResolution, ctx *commandLookupContext) resolverInspection {
	if !res.Found() {
		return resolverInspection{observationNotChecked[string](), observationNotChecked[string]()}
	}
	return resolverInspection{Version: r.version(res.Command()[0]), Signer: observationNotChecked[string]()}
}

// Version observation of one path, a vendor read failure naming its exception class
func (r PlatformAppResolver) version(path string) observation[string] {
	var obs observation[string]
	var err error
	switch kind := r.Definition.VersionKind; kind {
	case "none":
		return observationNotChecked[string]()
	case "plist":
		obs, err = platformPlistVersion(path)
	case "pe_resource":
		obs, err = platformPEVersion(path)
	case "uninstall_registry":
		obs, err = platformUninstallRegistryVersion(r.Definition.VersionArg)
	default:
		return observation[string]{State: checkUnavailable, Detail: "unknown version kind " + kind}
	}
	if err != nil {
		return observation[string]{State: checkError, Detail: pyExceptionClass(err)}
	}
	return obs
}

// Python exception class name of a vendor read failure
func pyExceptionClass(err error) string {
	if pe, ok := err.(*pyplist.Error); ok {
		return pe.Class
	}
	if os.IsNotExist(err) {
		return "FileNotFoundError"
	}
	if os.IsPermission(err) {
		return "PermissionError"
	}
	if pe, ok := err.(*os.PathError); ok && strings.Contains(pe.Err.Error(), "is a directory") {
		return "IsADirectoryError"
	}
	if cls, ok := err.(interface{ PyTypeName() string }); ok {
		return cls.PyTypeName()
	}
	return "OSError"
}

// Reads and validates the current runtime endpoint, false when the app is not live
func (r PlatformAppResolver) Endpoint() (PlatformEndpoint, bool, error) {
	d := r.Definition
	if d.LivenessKind != "server_json" {
		return PlatformEndpoint{}, false, nil
	}
	session := platformReadServerJSON(platformExpand(d.LivenessPath), d)
	if session == nil {
		return PlatformEndpoint{}, false, nil
	}
	alive, err := platformPIDAlive(session.PID)
	if err != nil {
		return PlatformEndpoint{}, false, err
	}
	if alive.Value == nil || !*alive.Value {
		return PlatformEndpoint{}, false, nil
	}
	endpoint := platformEndpointObservation(session.URL, d.EndpointPath)
	if endpoint.State != checkPresent || endpoint.Value == nil || *endpoint.Value == "" {
		return PlatformEndpoint{}, false, nil
	}
	return PlatformEndpoint{URL: *endpoint.Value, Token: session.Token}, true, nil
}

// Rereads the runtime file, connecting only for network effort with a live pid and loopback endpoint
func (r PlatformAppResolver) Probe(res commandResolution, effort ResolverEffort, deadline float64) (resolverProbe, error) {
	d := r.Definition
	nc := observationNotChecked[bool]()
	if d.LivenessKind != "server_json" {
		return resolverProbe{Running: nc, Answering: nc, Endpoint: observationNotChecked[string]()}, nil
	}
	session := platformReadServerJSON(platformExpand(d.LivenessPath), d)
	if session == nil {
		f := false
		absent := observation[bool]{State: checkAbsent, Value: &f, Detail: "runtime file missing or unreadable"}
		return resolverProbe{Running: absent, Answering: nc, Endpoint: observation[string]{State: checkAbsent}}, nil
	}
	running, err := platformPIDAlive(session.PID)
	if err != nil {
		return resolverProbe{}, err
	}
	endpoint := platformEndpointObservation(session.URL, d.EndpointPath)
	if effort == EffortLocal || running.Value == nil || !*running.Value || endpoint.State != checkPresent {
		return resolverProbe{Running: running, Answering: nc, Endpoint: endpoint}, nil
	}
	value := ""
	if endpoint.Value != nil {
		value = *endpoint.Value
	}
	return resolverProbe{Running: running, Answering: platformMCPInitialize(session, value, deadline), Endpoint: endpoint}, nil
}

type platformHit struct {
	Candidate commandCandidate
	Kind      string
}

type platformLocatorFunc func(d PlatformAppDef, loc PlatformAppLocation, ctx *commandLookupContext) []platformHit

// True when a bundle directory with an Info.plist or an executable file exists at path
func platformPresent(presence, path string) bool {
	if presence == "bundle" {
		st, err := os.Stat(path)
		if err != nil || !st.IsDir() {
			return false
		}
		info, err := os.Stat(filepath.Join(path, "Contents", "Info.plist"))
		return err == nil && info.Mode().IsRegular()
	}
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func platformHitAt(d PlatformAppDef, source, path string) platformHit {
	present := filepath.IsAbs(path) && platformPresent(d.Presence, path)
	return platformHit{commandCandidate{Value: path, Source: source, Present: present}, "known_path"}
}

var platformDigitRunRe = regexp.MustCompile(`\p{Nd}+`)

type platformOrderPart struct {
	digit bool
	num   *big.Int
	text  string
}

// Sort key splitting a path into text and integer runs so numeric folders order by value
func platformVersionOrder(path string) []platformOrderPart {
	var parts []platformOrderPart
	last := 0
	for _, m := range platformDigitRunRe.FindAllStringIndex(path, -1) {
		parts = append(parts, platformOrderPart{text: path[last:m[0]]})
		n := new(big.Int)
		for _, r := range path[m[0]:m[1]] {
			n.Mul(n, big.NewInt(10)).Add(n, big.NewInt(int64(unicodeDigit(r))))
		}
		parts = append(parts, platformOrderPart{digit: true, num: n})
		last = m[1]
	}
	return append(parts, platformOrderPart{text: path[last:]})
}

func unicodeDigit(r rune) int {
	if r >= '0' && r <= '9' {
		return int(r - '0')
	}
	for base := r; base > r-10 && base >= 0; base-- {
		if !unicode.Is(unicode.Nd, base-1) {
			return int(r - base)
		}
	}
	return 0
}

func comparePlatformOrder(a, b []platformOrderPart) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		x, y := a[i], b[i]
		if x.digit != y.digit {
			if x.digit {
				return 1
			}
			return -1
		}
		var c int
		if x.digit {
			c = x.num.Cmp(y.num)
		} else {
			c = strings.Compare(x.text, y.text)
			if c != 0 {
				c = comparePyStr(x.text, y.text)
			}
		}
		if c != 0 {
			return c
		}
	}
	return len(a) - len(b)
}

// Python str ordering by code point
func comparePyStr(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		if ra[i] != rb[i] {
			if ra[i] < rb[i] {
				return -1
			}
			return 1
		}
	}
	return len(ra) - len(rb)
}

// Matches a pattern whose only wildcard is star, listing directories like glob.glob over escaped parts
func platformGlob(pattern string) []string {
	vol := filepath.VolumeName(pattern)
	rest := pattern[len(vol):]
	sep := func(r rune) bool { return r == '/' || (runtime.GOOS == "windows" && r == '\\') }
	comps := strings.FieldsFunc(rest, sep)
	root := vol
	if len(rest) > 0 && sep(rune(rest[0])) {
		root += string(filepath.Separator)
	}
	paths := []string{root}
	for i, comp := range comps {
		last := i == len(comps)-1
		var next []string
		magic := strings.ContainsAny(comp, "*?[")
		for _, dir := range paths {
			if !magic {
				p := filepath.Join(dir, comp)
				if dir == "" {
					p = comp
				}
				if _, err := os.Lstat(p); err == nil {
					next = append(next, p)
				}
				continue
			}
			listDir := dir
			if listDir == "" {
				listDir = "."
			}
			entries, err := os.ReadDir(listDir)
			if err != nil {
				continue
			}
			escaped := platformGlobEscape(comp)
			for _, e := range entries {
				name := e.Name()
				if strings.HasPrefix(name, ".") && !strings.HasPrefix(comp, ".") {
					continue
				}
				if !fnmatch.Fnmatch(name, escaped) {
					continue
				}
				p := filepath.Join(dir, name)
				if !last {
					if st, err := os.Stat(p); err != nil || !st.IsDir() {
						continue
					}
				}
				next = append(next, p)
			}
		}
		paths = next
	}
	if len(comps) == 0 {
		return nil
	}
	return paths
}

// glob.escape of every star separated part joined by star, so star is the only wildcard
func platformGlobEscape(comp string) string {
	parts := strings.Split(comp, "*")
	for i, p := range parts {
		var b strings.Builder
		for _, r := range p {
			if r == '?' || r == '[' {
				b.WriteString("[" + string(r) + "]")
				continue
			}
			b.WriteRune(r)
		}
		parts[i] = b.String()
	}
	return strings.Join(parts, "*")
}

// Hits of a path location, star segments expanded highest version first
func platformPathHits(d PlatformAppDef, loc PlatformAppLocation, ctx *commandLookupContext) []platformHit {
	target, source := platformExpand(loc.Value), "app:"+d.AppID
	notFound := []platformHit{{commandCandidate{Value: target, Source: source}, "known_path"}}
	if !filepath.IsAbs(target) || strings.Contains(target, "%") || strings.Contains(target, "$") {
		return notFound
	}
	if !strings.Contains(target, "*") {
		return []platformHit{platformHitAt(d, source, target)}
	}
	matches := platformGlob(target)
	keys := make(map[string][]platformOrderPart, len(matches))
	for _, m := range matches {
		keys[m] = platformVersionOrder(m)
	}
	slices.SortStableFunc(matches, func(a, b string) int { return comparePlatformOrder(keys[b], keys[a]) })
	var hits []platformHit
	for _, m := range matches {
		hits = append(hits, platformHitAt(d, source, m))
	}
	if len(hits) == 0 {
		return notFound
	}
	return hits
}

// Hits of a command location, one per PATH candidate
func platformCommandHits(d PlatformAppDef, loc PlatformAppLocation, ctx *commandLookupContext) []platformHit {
	res := locateCommand(loc.Value, ctx, nil)
	var hits []platformHit
	for _, c := range res.Candidates {
		hits = append(hits, platformHit{commandCandidate{Value: c.Value, Source: "app:" + d.AppID + ":" + loc.Kind, Present: c.Present}, "path_executable"})
	}
	return hits
}

// Hits under the InstallLocation of each matching uninstall entry, Windows only and never a UNC share
func platformUninstallHits(d PlatformAppDef, loc PlatformAppLocation, ctx *commandLookupContext) []platformHit {
	if runtime.GOOS != "windows" {
		return nil
	}
	var hits []platformHit
	for _, entry := range platformUninstallEntries(loc.Value) {
		install := entry["InstallLocation"]
		if install == "" || strings.HasPrefix(install, `\\`) || strings.HasPrefix(install, "//") {
			continue
		}
		hits = append(hits, platformHitAt(d, "app:"+d.AppID+":"+loc.Kind, osPathJoin(install, loc.File)))
	}
	return hits
}

// Locator joining the value onto every directory of a known dir table
func platformInDirs(dirs func() []string) platformLocatorFunc {
	return func(d PlatformAppDef, loc PlatformAppLocation, ctx *commandLookupContext) []platformHit {
		var hits []platformHit
		for _, root := range dirs() {
			hits = append(hits, platformHitAt(d, "app:"+d.AppID+":"+loc.Kind, osPathJoin(platformExpand(root), loc.Value)))
		}
		return hits
	}
}

// platformLocationSpec is a mapping location kind with its OS, value key, presence and locator
type platformLocationSpec struct {
	OnlyOn    string
	Key       string
	Yields    string
	Locate    platformLocatorFunc
	NeedsFile bool
}

var platformLocationKinds map[string]platformLocationSpec

func init() {
	platformLocationKinds = map[string]platformLocationSpec{
		"command":            {"", "name", "executable", platformCommandHits, false},
		"uninstall_registry": {"win32", "display_name_prefix", "executable", platformUninstallHits, true},
		"app_bundle":         {"darwin", "name", "bundle", platformInDirs(macApplicationDirs), false},
		"flatpak":            {"linux", "app_id", "executable", platformInDirs(flatpakExportDirs), false},
		"snap":               {"linux", "name", "executable", platformInDirs(snapBinDirs), false},
	}
}

// Locator of a location kind, a plain path for kind path
func platformLocator(kind string) platformLocatorFunc {
	if kind == "path" {
		return platformPathHits
	}
	return platformLocationKinds[kind].Locate
}

// Bundle short version or bundle version from Contents/Info.plist
func platformPlistVersion(bundle string) (observation[string], error) {
	data, err := os.ReadFile(filepath.Join(bundle, "Contents", "Info.plist"))
	if err != nil {
		return observation[string]{}, err
	}
	info, err := pyplist.Load(data)
	if err != nil {
		return observation[string]{}, err
	}
	d, ok := info.(*pyplist.Dict)
	if !ok {
		return observation[string]{}, &pyplist.Error{Class: "AttributeError", Msg: fmt.Sprintf("object has no attribute 'get'")}
	}
	value, _ := d.Get("CFBundleShortVersionString")
	if !pyTruthy(platformPlistTruthValue(value)) {
		value, _ = d.Get("CFBundleVersion")
	}
	if !pyTruthy(platformPlistTruthValue(value)) {
		return observation[string]{State: checkUnavailable, Detail: "no version key in Info.plist"}, nil
	}
	s := pyplist.Str(value)
	return observation[string]{State: checkPresent, Value: &s}, nil
}

// Truthiness view of plist values, a dict by its length and a date always true
func platformPlistTruthValue(v any) any {
	switch x := v.(type) {
	case *pyplist.Dict:
		return x.Len() > 0
	case pyplist.Date, pyplist.UID:
		return true
	case *big.Int:
		return x.Sign() != 0
	}
	return v
}

// Uninstall entry DisplayVersion of the first entry with the display name prefix
func platformUninstallRegistryVersion(prefix string) (observation[string], error) {
	if runtime.GOOS != "windows" {
		return observation[string]{State: checkUnavailable, Detail: "uninstall_registry needs Windows"}, nil
	}
	entries := platformUninstallEntries(prefix)
	if len(entries) == 0 {
		return observation[string]{State: checkAbsent, Detail: "no uninstall entry"}, nil
	}
	v, ok := entries[0]["DisplayVersion"]
	if !ok {
		return observation[string]{State: checkUnavailable, Detail: "entry has no DisplayVersion"}, nil
	}
	return observation[string]{State: checkPresent, Value: &v}, nil
}

// platformSession is a parsed vendor runtime file whose token never prints
type platformSession struct {
	PID   *big.Int
	URL   string
	Token string
}

func (s platformSession) String() string {
	pid := "None"
	if s.PID != nil {
		pid = s.PID.String()
	}
	return fmt.Sprintf("_Session(pid=%s, url=%s, token=<redacted>)", pid, pyrepr.StrRepr(s.URL))
}

func (s platformSession) GoString() string { return s.String() }

// Runtime file keys, nil when the file is missing, unreadable, not JSON or not an object
func platformReadServerJSON(path string, d PlatformAppDef) *platformSession {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text, err := decodeStrictUTF8(raw)
	if err != nil {
		return nil
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		return nil
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return nil
	}
	s := &platformSession{}
	pid, _ := obj.Get(d.LivenessPIDKey)
	switch v := pid.(type) {
	case bool:
		if v {
			s.PID = big.NewInt(1)
		} else {
			s.PID = big.NewInt(0)
		}
	case int64:
		s.PID = big.NewInt(v)
	case *big.Int:
		s.PID = v
	case big.Int:
		s.PID = &v
	}
	if u, ok := obj.Get(d.LivenessURLKey); ok {
		s.URL, _ = u.(string)
	}
	if t, ok := obj.Get(d.LivenessTokenKey); ok {
		s.Token, _ = t.(string)
	}
	return s
}

// UTF-8 text of file bytes, an error where Python's utf-8 codec raises
func decodeStrictUTF8(raw []byte) (string, error) {
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("invalid utf-8")
	}
	return string(raw), nil
}

// Loopback http endpoint with a numeric port and no userinfo, the path replaced by ours
func platformEndpointObservation(rawURL, fixedPath string) observation[string] {
	if rawURL == "" {
		return observation[string]{State: checkUnavailable, Detail: "no url in runtime file"}
	}
	parts, err := pyurl.Split(rawURL)
	if err != nil {
		return observation[string]{State: checkUnavailable, Detail: "malformed endpoint"}
	}
	portNum, hasPort, portErr := parts.Port()
	if portErr != nil {
		return observation[string]{State: checkUnavailable, Detail: "malformed endpoint"}
	}
	hostname := parts.Hostname()
	username, password := "", ""
	if at := strings.LastIndex(parts.Netloc, "@"); at >= 0 {
		username, password, _ = strings.Cut(parts.Netloc[:at], ":")
	}
	if parts.Scheme != "http" || username != "" || password != "" {
		return observation[string]{State: checkUnavailable, Detail: "endpoint must be plain http without userinfo"}
	}
	if !slices.Contains(platformLoopbackHosts, hostname) {
		return observation[string]{State: checkUnavailable, Detail: "endpoint must be loopback"}
	}
	if !hasPort || portNum < 1 || portNum > 65535 {
		return observation[string]{State: checkUnavailable, Detail: "endpoint needs a numeric port"}
	}
	value := fmt.Sprintf("http://%s:%d%s", hostname, portNum, fixedPath)
	return observation[string]{State: checkPresent, Value: &value}
}

// One MCP initialize POST under one absolute deadline covering connect and response headers
func platformMCPInitialize(session *platformSession, endpoint string, deadline float64) observation[bool] {
	parts, _ := pyurl.Split(endpoint)
	body := `{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "icebath", "version": "probe"}}}`
	limit := time.Duration(max(0.1, deadline) * float64(time.Second))
	host := parts.Hostname()
	if host == "" {
		host = "127.0.0.1"
	}
	port := 80
	if p, ok, err := parts.Port(); err == nil && ok {
		port = p
	}
	path := parts.Path
	if path == "" {
		path = "/"
	}
	f, t := false, true
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{}).DialContext}
	client := &http.Client{Transport: transport, Timeout: limit, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(http.MethodPost, "http://"+net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port))+path, bytes.NewReader([]byte(body)))
	if err != nil {
		return observation[bool]{State: checkAbsent, Value: &f, Detail: "connection refused or timed out"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session.Token != "" {
		req.Header.Set("Authorization", "Bearer "+session.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return observation[bool]{State: checkAbsent, Value: &f, Detail: fmt.Sprintf("no answer within %s s", pyFormatG(deadline))}
		}
		return observation[bool]{State: checkAbsent, Value: &f, Detail: "connection refused or timed out"}
	}
	resp.Body.Close()
	status := resp.StatusCode
	if status == 200 || status == 401 || status == 403 {
		return observation[bool]{State: checkPresent, Value: &t, Detail: fmt.Sprintf("http %d", status)}
	}
	return observation[bool]{State: checkAbsent, Value: &f, Detail: fmt.Sprintf("http %d", status)}
}

// Python format(x, 'g')
func pyFormatG(x float64) string {
	s := strconv.FormatFloat(x, 'g', 6, 64)
	if strings.Contains(s, "e") {
		mant, exp, _ := strings.Cut(s, "e")
		sign := exp[0]
		digits := strings.TrimLeft(exp[1:], "0")
		if len(digits) < 2 {
			digits = strings.Repeat("0", 2-len(digits)) + digits
		}
		return mant + "e" + string(sign) + digits
	}
	return s
}

// PlatformAvailability is whether a declaration can be offered on this host
type PlatformAvailability struct {
	State      string
	Version    *string
	Path       *string
	MinVersion *string
}

// True for available and no_requirements
func (a PlatformAvailability) Offerable() bool {
	return a.State == "available" || a.State == "no_requirements"
}

// Ordered state, version, path and min_version mapping, None for unset values
func (a PlatformAvailability) AsDict() *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("state", a.State)
	for _, kv := range []struct {
		k string
		v *string
	}{{"version", a.Version}, {"path", a.Path}, {"min_version", a.MinVersion}} {
		if kv.v == nil {
			o.Set(kv.k, nil)
		} else {
			o.Set(kv.k, *kv.v)
		}
	}
	return o
}

// platformRequirements is the slice of a catalog entry availability reads
type platformRequirements interface {
	RequiresApp() bool
	MinVersion() *string
	RequiredGPU() *string
	AppFor(osFamily string) (PlatformAppDef, bool)
}

var (
	platformOSFamily       = hostinfo.OSFamily
	platformGPUClass       = hostinfo.GPUClass
	platformVersionPartsRe = regexp.MustCompile(`^[0-9]{1,9}(?:\.[0-9]{1,9})*$`)
)

func platformVersionParts(version string) []int64 {
	if !platformVersionPartsRe.MatchString(version) {
		return nil
	}
	var out []int64
	for _, c := range strings.Split(version, ".") {
		n, _ := strconv.ParseInt(c, 10, 64)
		out = append(out, n)
	}
	return out
}

func compareIntLists(a, b []int64) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}

// Compares dot separated decimal components, failing closed on invalid input
func PlatformVersionAtLeast(found, minimum string) bool {
	a, b := platformVersionParts(found), platformVersionParts(minimum)
	if a == nil || b == nil {
		return false
	}
	width := max(len(a), len(b))
	for len(a) < width {
		a = append(a, 0)
	}
	for len(b) < width {
		b = append(b, 0)
	}
	return compareIntLists(a, b) >= 0
}

func platformStrPtr(s string) *string { return &s }

// Offer time verdict from locate, inspect and the cached GPU fact, osFamily empty meaning this host
func PlatformAvailabilityOf(entry platformRequirements, osFamily string) PlatformAvailability {
	gpu := entry.RequiredGPU()
	if !entry.RequiresApp() && gpu == nil {
		return PlatformAvailability{State: "no_requirements"}
	}
	osf := osFamily
	if osf == "" {
		osf = platformOSFamily()
	}
	var definition PlatformAppDef
	hasDef := false
	if entry.RequiresApp() {
		definition, hasDef = entry.AppFor(osf)
		if !hasDef {
			return PlatformAvailability{State: "unsupported_os", MinVersion: entry.MinVersion()}
		}
	}
	if gpu != nil {
		if class := platformGPUClass(); class != *gpu && class != "unknown" {
			return PlatformAvailability{State: "unsupported_gpu", MinVersion: entry.MinVersion()}
		}
	}
	if !hasDef {
		return PlatformAvailability{State: "no_requirements"}
	}
	resolver := PlatformAppResolver{definition}
	res := resolver.Locate(nil)
	var lookedAt *string
	if cmd := res.Command(); len(cmd) > 0 {
		lookedAt = platformStrPtr(cmd[0])
	} else if len(res.Candidates) > 0 {
		lookedAt = platformStrPtr(res.Candidates[0].Value)
	}
	if !res.Found() {
		return PlatformAvailability{State: "missing_app", Path: lookedAt, MinVersion: entry.MinVersion()}
	}
	if definition.VersionKind == "none" {
		return PlatformAvailability{State: "available", Path: lookedAt, MinVersion: entry.MinVersion()}
	}
	versionOf := func(copy commandCandidate) *string {
		obs := resolver.Inspect(commandResolution{Kind: res.Kind, Candidates: []commandCandidate{copy}}, nil).Version
		if obs.State == checkPresent {
			return obs.Value
		}
		return nil
	}
	minVersion := entry.MinVersion()
	if minVersion == nil || *minVersion == "" {
		return PlatformAvailability{State: "available", Version: versionOf(res.PresentCandidates()[0]), Path: lookedAt}
	}
	type copyVersion struct {
		path    string
		version *string
	}
	var copies []copyVersion
	for _, c := range res.PresentCandidates() {
		copies = append(copies, copyVersion{c.Value, versionOf(c)})
	}
	for _, c := range copies {
		if c.version != nil && PlatformVersionAtLeast(*c.version, *minVersion) {
			return PlatformAvailability{State: "available", Version: c.version, Path: platformStrPtr(c.path), MinVersion: minVersion}
		}
	}
	var best []int64
	path, version := lookedAt, (*string)(nil)
	for _, c := range copies {
		if c.version == nil {
			continue
		}
		parts := platformVersionParts(*c.version)
		if parts == nil {
			continue
		}
		if best == nil || compareIntLists(parts, best) > 0 {
			best, path, version = parts, platformStrPtr(c.path), c.version
		}
	}
	return PlatformAvailability{State: "version_too_old", Version: version, Path: path, MinVersion: minVersion}
}

// Skill requires_apps verdict of a name, declared only with an app block, offerable by availability
func PlatformSkillAppAvailability(name string) (declared bool, offerable bool) {
	decl := LookupPlatformDeclaration(name)
	if decl == nil || decl.App == nil {
		return false, false
	}
	return true, PlatformAvailabilityOf(decl, "").Offerable()
}
