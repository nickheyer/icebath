package codeexec

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	cuStatusGlyph        = map[string]string{"pass": "✅", "fail": "❌", "skip": "⏭️"}
	cuOverallGlyph       = map[string]string{"ok": "✅", "degraded": "⚠️", "failed": "❌"}
	cuSupportedPlatforms = []string{"darwin", "linux", "windows"}
	cuTCCCheckFields     = map[string]string{"tcc_accessibility": "accessibility", "tcc_screen_recording": "screen_recording"}
	cuVersionRe          = regexp.MustCompile(`(\p{Nd}+\.\p{Nd}+\.\p{Nd}+(?:[-+][\p{L}\p{N}_.]+)?)`)
	cuVersionTokenRe     = regexp.MustCompile(`(\p{Nd}+\.\p{Nd}+(?:\.\p{Nd}+)?(?:[-+][\p{L}\p{N}_.]+)?)`)
)

const (
	cuTCCHint         = "Grant %s to CuaDriver in System Settings → Privacy & Security."
	cuZeroDisplayMsg  = "ScreenCaptureKit reachable but 0 shareable display(s) — every capture will return 0x0."
	cuZeroDisplayHint = "Wake the built-in display, connect a monitor or HDMI dummy dongle (e.g. Headless Ghost), or enable " +
		"a virtual display (Screen Sharing/VNC, BetterDisplay). Verify with `system_profiler SPDisplaysDataType`."
	cuPrunedUnitMsg  = "Exec targets %s which no longer exists — a cua-driver upgrade pruned that release directory"
	cuPrunedUnitHint = "Point the %s at ~/.cua-driver/packages/current/cua-driver — `current` survives upgrades; " +
		"versioned packages/releases/<version>/ dirs are pruned (only the last 5 are kept)"
	cuDeadDaemonMsg = "%s is configured to run `cua-driver serve` but no daemon is listening on %s — " +
		"the unit is not running (crash loop, stopped, or never started)"
	cuDeadDaemonHint = "Check `systemctl --user status %s` / `journalctl --user -u %s`; a driver reinstall does not " +
		"start the daemon and cannot fix a broken unit"
)

// healthReportUnavailable is the denied or non schema health_report the doctor falls back from
type healthReportUnavailable struct{ msg string }

func (e *healthReportUnavailable) Error() string { return e.msg }

// doctorRuntimeError is the RuntimeError of a failed MCP exchange
type doctorRuntimeError struct{ msg string }

func (e *doctorRuntimeError) Error() string { return e.msg }

// Captured run of the driver with the sanitized env, an IO failure returned as the error
func cliText(env *tools.Env, binary string, timeout float64, args ...string) (*completedProcess, error) {
	return permissionsRun(env, binary, timeout, args...)
}

// Stripped stdout plus stderr
func combinedOutput(cp *completedProcess) string { return pystr.Strip(cp.Stdout + cp.Stderr) }

// First stripped line of the text, nil when blank
func firstLine(text string) *string {
	if pystr.Strip(text) == "" {
		return nil
	}
	l := pystr.Strip(pystr.Splitlines(pystr.Strip(text), false)[0])
	return &l
}

// First line of the driver version output, nil when the run failed
func readCLIVersion(env *tools.Env, binary string) *string {
	cp, err := cliText(env, binary, 5, "--version")
	if err != nil {
		return nil
	}
	if cp.Stdout != "" {
		return firstLine(cp.Stdout)
	}
	return firstLine(cp.Stderr)
}

// Status and version or failure message of the driver version verb
func cliDriverVersion(env *tools.Env, binary string) (string, string) {
	cp, err := cliText(env, binary, 5, "--version")
	if err != nil {
		return "fail", "--version failed: " + err.Error()
	}
	text, failed := combinedOutput(cp), cp.Returncode != 0
	if failed && text == "" {
		return "fail", fmt.Sprintf("--version exited %d", cp.Returncode)
	}
	status := "pass"
	if failed {
		status = "fail"
	}
	if m := cuVersionRe.FindStringSubmatch(text); m != nil {
		return status, m[1]
	}
	if l := firstLine(text); l != nil {
		return status, *l
	}
	return status, "unknown"
}

// Output of the driver doctor verb, nil on failure or silence
func cliDoctorSnippet(env *tools.Env, binary string) *string {
	cp, err := cliText(env, binary, 8, "doctor")
	if err != nil {
		return nil
	}
	if out := combinedOutput(cp); out != "" {
		return &out
	}
	return nil
}

// Dotted version token of free text, else the lowercased stripped text
func versionToken(text string) string {
	if text != "" {
		if m := cuVersionTokenRe.FindStringSubmatch(text); m != nil {
			return m[1]
		}
	}
	return pystr.Lower(pystr.Strip(text))
}

// Identity block comparing the resolved binary's own version with the health report's
func buildIdentity(env *tools.Env, binary string, report *pyjson.Object) *pyjson.Object {
	cli := ""
	if v := readCLIVersion(env, binary); v != nil {
		cli = *v
	}
	reportV := ""
	if v := report.Vals["driver_version"]; pyrepr.Truthy(v) {
		reportV = pyrepr.Str(v)
	}
	o := pyjson.NewObject()
	o.Set("resolved_binary", binary)
	o.Set("cli_version", orNil(cli))
	o.Set("health_report_driver_version", orNil(reportV))
	tc, tr := versionToken(cli), versionToken(reportV)
	o.Set("version_mismatch", tc != "" && tr != "" && tc != tr)
	return o
}

// The string, nil when empty
func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// True for a mapping with schema_version, overall and a checks list
func isValidHealthReport(payload any) bool {
	o, ok := payload.(*pyjson.Object)
	if !ok {
		return false
	}
	_, hasSchema := o.Vals["schema_version"]
	_, hasOverall := o.Vals["overall"]
	_, isList := o.Vals["checks"].([]any)
	return hasSchema && hasOverall && isList
}

// Text of every text content item of a tools/call result
func textItems(result *pyjson.Object) []string {
	var out []string
	items, _ := result.Vals["content"].([]any)
	for _, it := range items {
		if o, ok := it.(*pyjson.Object); ok && o.Vals["type"] == "text" {
			if s, ok := o.Vals["text"].(string); ok {
				out = append(out, s)
			} else if pyrepr.Truthy(o.Vals["text"]) {
				out = append(out, pyrepr.Str(o.Vals["text"]))
			} else {
				out = append(out, "")
			}
		}
	}
	return out
}

// First non blank stripped text item, else the default
func firstText(result *pyjson.Object, def string) string {
	for _, t := range textItems(result) {
		if s := pystr.Strip(t); s != "" {
			return s
		}
	}
	return def
}

// Health report of a tools/call result, unavailable when denied or not a real report
func extractHealthReportFromResult(result *pyjson.Object) (*pyjson.Object, error) {
	if result.Vals["isError"] == true {
		return nil, &healthReportUnavailable{firstText(result, "health_report returned isError=true")}
	}
	sc := result.Vals["structuredContent"]
	if isValidHealthReport(sc) {
		return sc.(*pyjson.Object), nil
	}
	for _, text := range textItems(result) {
		if parsed, err := pyjson.Loads(text); err == nil && isValidHealthReport(parsed) {
			return parsed.(*pyjson.Object), nil
		}
	}
	if o, ok := sc.(*pyjson.Object); ok {
		keys := append([]string(nil), o.Keys...)
		sort.Strings(keys)
		ks := make([]any, len(keys))
		for i, k := range keys {
			ks[i] = k
		}
		return nil, &healthReportUnavailable{"health_report structuredContent lacks schema_version/overall/checks (keys=" + pyrepr.Repr(ks) + ")"}
	}
	ks := make([]any, len(result.Keys))
	for i, k := range result.Keys {
		ks[i] = k
	}
	return nil, &doctorRuntimeError{"health_report response carried neither structuredContent nor a parseable JSON text block. Result keys: " + pyrepr.Repr(ks)}
}

type cuMCPProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr io.Reader
}

// Spawns the driver mcp verb with piped stdio and the sanitized env, a spawn failure being an OSError
var openMCP = func(env *tools.Env, binary string) (*cuMCPProc, error) {
	cmd := exec.Command(binary, "mcp")
	cmd.Env = envList(permissionsChildEnv(env))
	hideConsoleWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, &spawnError{err}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &spawnError{err}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, &spawnError{err}
	}
	if err := cmd.Start(); err != nil {
		return nil, &spawnError{err}
	}
	return &cuMCPProc{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), stderr: stderr}, nil
}

// Writes one JSON-RPC request and reads one response line
func mcpRPC(p *cuMCPProc, msgID int, method string, params any) (*pyjson.Object, error) {
	payload := pyjson.NewObject()
	payload.Set("jsonrpc", "2.0")
	payload.Set("id", msgID)
	payload.Set("method", method)
	if params != nil {
		payload.Set("params", params)
	}
	text, err := pyjson.Dumps(payload)
	if err != nil {
		return nil, err
	}
	nl := "\n"
	if runtime.GOOS == "windows" {
		nl = "\r\n"
	}
	if _, err := io.WriteString(p.stdin, text+nl); err != nil {
		return nil, &spawnError{err}
	}
	raw, _ := p.stdout.ReadString('\n')
	line := textModeDecode([]byte(raw))
	if line == "" {
		tail := "(empty)"
		if b, err := io.ReadAll(p.stderr); err == nil {
			lines := pystr.Splitlines(pystr.Strip(textModeDecode(b)), false)
			lines = lines[max(0, len(lines)-3):]
			if len(lines) > 0 {
				items := make([]any, len(lines))
				for i, l := range lines {
					items[i] = l
				}
				tail = pyrepr.Repr(items)
			}
		}
		return nil, &doctorRuntimeError{fmt.Sprintf("cua-driver mcp produced no response for %s. stderr tail: %s", pyrepr.Repr(method), tail)}
	}
	decoded, err := pyjson.Loads(line)
	if err != nil {
		return nil, &doctorRuntimeError{fmt.Sprintf("%s response was not valid JSON: %v\nraw: %s", method, err, pyPrefix(line, 200))}
	}
	resp, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, &AttributeError{"'" + pyrepr.TypeName(decoded) + "' object has no attribute 'get'"}
	}
	if e, has := resp.Vals["error"]; has {
		return nil, &doctorRuntimeError{fmt.Sprintf("%s JSON-RPC error: %s", method, pyrepr.Str(e))}
	}
	return resp, nil
}

// Raw result of a tools/call, an empty mapping when absent
func callTool(p *cuMCPProc, msgID int, name string, arguments *pyjson.Object) (any, error) {
	if arguments == nil {
		arguments = pyjson.NewObject()
	}
	params := pyjson.NewObject()
	params.Set("name", name)
	params.Set("arguments", arguments)
	resp, err := mcpRPC(p, msgID, "tools/call", params)
	if err != nil {
		return nil, err
	}
	if r := resp.Vals["result"]; pyrepr.Truthy(r) {
		return r, nil
	}
	return pyjson.NewObject(), nil
}

// Runs fn in an MCP session, closing stdin and waiting out or killing the child afterwards
func withMCPSession(env *tools.Env, binary string, timeout float64, fn func(*cuMCPProc) error) error {
	p, err := openMCP(env, binary)
	if err != nil {
		return err
	}
	defer func() {
		_ = p.stdin.Close()
		done := make(chan struct{})
		go func() { _ = p.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Duration(timeout * float64(time.Second))):
			_ = p.cmd.Process.Kill()
			<-done
		}
	}()
	return fn(p)
}

// Handshake then health_report, unavailable for the fallback or a runtime failure
func driveHealthReport(env *tools.Env, binary string, include, skip []string, timeout float64) (*pyjson.Object, error) {
	args := pyjson.NewObject()
	for _, kv := range []struct {
		k string
		v []string
	}{{"include", include}, {"skip", skip}} {
		if len(kv.v) > 0 {
			l := make([]any, len(kv.v))
			for i, s := range kv.v {
				l[i] = s
			}
			args.Set(kv.k, l)
		}
	}
	var result any
	err := withMCPSession(env, binary, timeout, func(p *cuMCPProc) error {
		if _, err := mcpRPC(p, 1, "initialize", pyjson.NewObject()); err != nil {
			return err
		}
		r, err := callTool(p, 2, "health_report", args)
		result = r
		return err
	})
	if err != nil {
		return nil, err
	}
	o, ok := result.(*pyjson.Object)
	if !ok {
		return nil, &doctorRuntimeError{"health_report result was not an object: " + pyrepr.TypeName(result)}
	}
	return extractHealthReportFromResult(o)
}

// Structured content of a result, empty when absent
func structuredOf(result *pyjson.Object) *pyjson.Object {
	if o, ok := result.Vals["structuredContent"].(*pyjson.Object); ok {
		return o
	}
	return pyjson.NewObject()
}

// Result of one probe tool, or the error text on isError or RPC failure
func probeTool(p *cuMCPProc, msgID int, name string) (*pyjson.Object, *string, error) {
	r, err := callTool(p, msgID, name, nil)
	if err != nil {
		var rt *doctorRuntimeError
		if errors.As(err, &rt) {
			msg := rt.Error()
			return nil, &msg, nil
		}
		return nil, nil, err
	}
	result, ok := r.(*pyjson.Object)
	if !ok {
		return nil, nil, &AttributeError{"'" + pyrepr.TypeName(r) + "' object has no attribute 'get'"}
	}
	if result.Vals["isError"] == true {
		msg := firstText(result, name+" isError")
		return nil, &msg, nil
	}
	return result, nil, nil
}

type fallbackProbes struct {
	initVersion      any
	permissions      *pyjson.Object
	permissionsError *string
	listAppsOK       *bool
	listAppsError    *string
	listAppsCount    *int
}

// One MCP session reading the initialize version, check_permissions and a list_apps probe
func driveFallbackProbes(env *tools.Env, binary string, timeout float64) (*fallbackProbes, error) {
	out := &fallbackProbes{}
	err := withMCPSession(env, binary, timeout, func(p *cuMCPProc) error {
		init, err := mcpRPC(p, 1, "initialize", pyjson.NewObject())
		if err != nil {
			return err
		}
		if res, ok := init.Vals["result"].(*pyjson.Object); ok {
			if info, ok := res.Vals["serverInfo"].(*pyjson.Object); ok {
				out.initVersion = info.Vals["version"]
			}
		}
		perms, permErr, err := probeTool(p, 2, "check_permissions")
		if err != nil {
			return err
		}
		out.permissionsError = permErr
		if perms != nil {
			out.permissions = structuredOf(perms)
		}
		apps, appsErr, err := probeTool(p, 3, "list_apps")
		if err != nil {
			return err
		}
		out.listAppsError = appsErr
		ok := apps != nil
		out.listAppsOK = &ok
		if apps != nil {
			if l, isList := structuredOf(apps).Vals["apps"].([]any); isList {
				n := len(l)
				out.listAppsCount = &n
			}
		}
		return nil
	})
	return out, err
}

// Lowercased Python platform.system, unknown when empty
var doctorPlatformName = func() string {
	if s := pystr.Lower(pyplatform.System()); s != "" {
		return s
	}
	return "unknown"
}

type doctorRow struct {
	status, message string
	hint            *string
	data            *pyjson.Object
}

type doctorCtx struct {
	plat, verStatus, verMsg, reasonShort string
	probes                               *fallbackProbes
	perms                                *pyjson.Object
	permErr                              *string
	axGranted                            bool
	doctorTxt                            *string
}

// Row of one check_permissions boolean field
func tccRow(field, label string, platformBound bool, c *doctorCtx) *doctorRow {
	if c.perms == nil {
		if c.permErr != nil && *c.permErr != "" {
			return &doctorRow{status: "fail", message: *c.permErr}
		}
		return &doctorRow{status: "skip", message: "check_permissions unavailable"}
	}
	granted, isBool := c.perms.Vals[field].(bool)
	if !isBool {
		if platformBound && c.plat != "darwin" {
			return &doctorRow{status: "skip", message: "not applicable on " + c.plat}
		}
		return &doctorRow{status: "skip", message: field + " field absent from check_permissions"}
	}
	if !granted {
		hint := fmt.Sprintf(cuTCCHint, label)
		data := pyjson.NewObject()
		data.Set(field, false)
		return &doctorRow{status: "fail", message: label + " is not granted.", hint: &hint, data: data}
	}
	data := pyjson.NewObject()
	data.Set(field, true)
	if field == "screen_recording" {
		data.Set("screen_recording_capturable", c.perms.Vals["screen_recording_capturable"])
		if data.Vals["screen_recording_capturable"] == false {
			hint := "Screen Recording permission may need a restart of CuaDriver or a re-grant in System Settings."
			return &doctorRow{status: "fail", message: "Screen Recording granted but not capturable.", hint: &hint, data: data}
		}
	}
	return &doctorRow{status: "pass", message: label + " is granted.", data: data}
}

// Accessibility capability from the list_apps probe or the accessibility grant
func axCapabilityRow(c *doctorCtx) *doctorRow {
	p := c.probes
	if p.listAppsOK != nil && *p.listAppsOK {
		msg := "list_apps succeeded"
		if p.listAppsCount != nil {
			msg += fmt.Sprintf(" (%d apps)", *p.listAppsCount)
		}
		return &doctorRow{status: "pass", message: msg}
	}
	if p.listAppsOK != nil {
		if p.listAppsError != nil && *p.listAppsError != "" {
			return &doctorRow{status: "fail", message: *p.listAppsError}
		}
		msg := "list_apps failed"
		if c.axGranted {
			msg += " despite accessibility grant"
		}
		return &doctorRow{status: "fail", message: msg}
	}
	if c.axGranted {
		return &doctorRow{status: "pass", message: "inferred from accessibility grant (list_apps not probed)"}
	}
	return &doctorRow{status: "skip", message: "not probed"}
}

// Row of the driver doctor text, nil when there is none
func cliDoctorRow(txt *string) *doctorRow {
	if txt == nil || *txt == "" {
		return nil
	}
	status := "skip"
	if strings.Contains(pystr.Lower(*txt), "[ok") || strings.Contains(*txt, "ok  ]") {
		status = "pass"
	}
	data := pyjson.NewObject()
	data.Set("snippet", pyPrefix(*txt, 2000))
	return &doctorRow{status: status, message: pystr.Strip(pystr.Splitlines(*txt, false)[0]), data: data}
}

var fallbackProbeTable = []struct {
	name  string
	build func(*doctorCtx) *doctorRow
}{
	{"binary_version", func(c *doctorCtx) *doctorRow { return &doctorRow{status: c.verStatus, message: c.verMsg} }},
	{"platform_supported", func(c *doctorCtx) *doctorRow {
		for _, p := range cuSupportedPlatforms {
			if p == c.plat {
				return &doctorRow{status: "pass", message: "platform=" + c.plat}
			}
		}
		return &doctorRow{status: "fail", message: "platform=" + c.plat + " (unsupported)"}
	}},
	{"session_active", func(*doctorCtx) *doctorRow {
		return &doctorRow{status: "skip", message: "not probed (doctor does not open a cua session)"}
	}},
	{"tcc_accessibility", func(c *doctorCtx) *doctorRow { return tccRow("accessibility", "Accessibility", false, c) }},
	{"tcc_screen_recording", func(c *doctorCtx) *doctorRow { return tccRow("screen_recording", "Screen Recording", true, c) }},
	{"ax_capability", axCapabilityRow},
	{"health_report_path", func(c *doctorCtx) *doctorRow {
		return &doctorRow{status: "skip", message: "fallback composite (cua-driver 0.10 unclassified health_report); cause: " + c.reasonShort}
	}},
	{"cli_doctor", func(c *doctorCtx) *doctorRow { return cliDoctorRow(c.doctorTxt) }},
}

// Failed without a passing binary, ok when accessibility is fine and nothing failed, else degraded
func overallFrom(checks []any) string {
	byName := map[any]any{}
	anyFail := false
	for _, c := range checks {
		o := c.(*pyjson.Object)
		byName[o.Vals["name"]] = o.Vals["status"]
		anyFail = anyFail || o.Vals["status"] == "fail"
	}
	if byName["binary_version"] != "pass" {
		return "failed"
	}
	if tcc := byName["tcc_accessibility"]; (tcc == "pass" || tcc == "skip" || tcc == nil) && !anyFail {
		return "ok"
	}
	return "degraded"
}

// Schema version one report synthesized from CLI and MCP probes when health_report is denied
func composeFallbackReport(env *tools.Env, binary, reason string, timeout float64) (*pyjson.Object, error) {
	plat := doctorPlatformName()
	verStatus, verValue := cliDriverVersion(env, binary)
	probes, err := driveFallbackProbes(env, binary, timeout)
	if err != nil {
		return nil, err
	}
	if pyrepr.Truthy(probes.initVersion) {
		verStatus, verValue = "pass", pyrepr.Str(probes.initVersion)
	}
	reasonShort := reason
	if reasonShort == "" {
		reasonShort = "health_report unavailable"
	}
	reasonShort = pystr.Strip(reasonShort)
	if pystr.Len(reasonShort) > 160 {
		reasonShort = pyPrefix(reasonShort, 157) + "..."
	}
	verMsg := verValue
	if verStatus == "pass" {
		verMsg = "cua-driver " + verValue
	} else if verMsg == "" {
		verMsg = "version unknown"
	}
	c := &doctorCtx{plat: plat, verStatus: verStatus, verMsg: verMsg, reasonShort: reasonShort, probes: probes,
		perms: probes.permissions, permErr: probes.permissionsError,
		axGranted: probes.permissions != nil && probes.permissions.Vals["accessibility"] == true,
		doctorTxt: cliDoctorSnippet(env, binary)}
	checks := []any{}
	for _, p := range fallbackProbeTable {
		row := p.build(c)
		if row == nil {
			continue
		}
		o := pyjson.NewObject()
		o.Set("name", p.name)
		o.Set("status", row.status)
		o.Set("message", row.message)
		if row.hint != nil {
			o.Set("hint", *row.hint)
		}
		if row.data != nil {
			o.Set("data", row.data)
		}
		checks = append(checks, o)
	}
	driver := verValue
	if verStatus != "pass" && driver == "" {
		driver = "?"
	}
	if reason == "" {
		reason = "health_report unavailable"
	}
	report := pyjson.NewObject()
	report.Set("schema_version", "1")
	report.Set("platform", plat)
	report.Set("driver_version", driver)
	report.Set("overall", overallFrom(checks))
	report.Set("checks", checks)
	report.Set("fallback", true)
	report.Set("fallback_reason", reason)
	return report, nil
}

// Check mappings of the report's checks list
func reportChecks(report *pyjson.Object) ([]*pyjson.Object, bool) {
	l, ok := report.Vals["checks"].([]any)
	var out []*pyjson.Object
	for _, c := range l {
		if o, isObj := c.(*pyjson.Object); isObj {
			out = append(out, o)
		}
	}
	return out, ok
}

// Appends the stale grant recovery hint to every failed tcc check
func applyStaleTCCGuard(report *pyjson.Object) *pyjson.Object {
	checks, _ := reportChecks(report)
	for _, c := range checks {
		name, _ := c.Vals["name"].(string)
		if field := cuTCCCheckFields[name]; field != "" && c.Vals["status"] == "fail" {
			prev := ""
			if h := c.Vals["hint"]; pyrepr.Truthy(h) {
				prev = pyrepr.Str(h)
			}
			c.Set("hint", pystr.Strip(prev+" "+staleTCCGrantHint(field)))
		}
	}
	return report
}

// Fails a passing screen capture check that reports zero displays and degrades an ok overall
func applyDisplayCountGuard(report *pyjson.Object) *pyjson.Object {
	checks, _ := reportChecks(report)
	for _, c := range checks {
		if c.Vals["name"] != "screen_capture_capability" {
			continue
		}
		var count any
		if d, ok := c.Vals["data"].(*pyjson.Object); ok {
			count = d.Vals["display_count"]
		}
		if cuIsZero(count) && c.Vals["status"] == "pass" {
			c.Set("status", "fail")
			c.Set("message", cuZeroDisplayMsg)
			c.Set("hint", cuZeroDisplayHint)
			if report.Vals["overall"] == "ok" {
				report.Set("overall", "degraded")
			}
		}
	}
	return report
}

// Python equality with zero for a JSON number or bool
func cuIsZero(v any) bool {
	switch n := v.(type) {
	case bool:
		return !n
	case int:
		return n == 0
	case int64:
		return n == 0
	case float64:
		return n == 0
	}
	return pyrepr.TypeName(v) == "int" && pyrepr.Str(v) == "0"
}

type cuDaemonUnit struct {
	kind, unit, execTarget string
	runsServe              bool
	socket                 *string
}

// User home the way expanduser of a tilde reads it
func doctorUserHome() string {
	if h := os.Getenv("HOME"); h != "" && runtime.GOOS != "windows" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// Tilde expansion of a leading tilde path against the user home
func doctorExpandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || (runtime.GOOS == "windows" && strings.HasPrefix(p, `~\`)) {
		return doctorUserHome() + p[1:]
	}
	return p
}

// Every systemd user unit and XDG autostart entry whose Exec runs cua-driver, with its serve flag and socket
func cuaDaemonUnits(configDir string) []cuDaemonUnit {
	userHome := doctorUserHome()
	base := configDir
	if base == "" {
		base = os.Getenv("XDG_CONFIG_HOME")
	}
	if base == "" {
		base = filepath.Join(userHome, ".config")
	}
	var units []cuDaemonUnit
	for _, src := range []struct{ kind, dir, suffix, key string }{
		{"systemd user unit", filepath.Join(base, "systemd", "user"), ".service", "ExecStart"},
		{"XDG autostart entry", filepath.Join(base, "autostart"), ".desktop", "Exec"},
	} {
		entries, err := os.ReadDir(src.dir)
		if err != nil {
			continue
		}
		var names []string
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), src.suffix) {
				continue
			}
			if st, err := os.Stat(filepath.Join(src.dir, e.Name())); err == nil && st.Mode().IsRegular() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			raw, err := os.ReadFile(filepath.Join(src.dir, name))
			if err != nil {
				continue
			}
			text := strings.TrimPrefix(textModeDecode(raw), "\uFEFF")
			for _, line := range pystr.Splitlines(text, false) {
				stripped := pystr.Strip(line)
				var tokens []string
				if strings.HasPrefix(stripped, src.key+"=") {
					_, rest, _ := strings.Cut(stripped, "=")
					tokens = strings.FieldsFunc(rest, pystr.IsSpace)
				}
				if len(tokens) == 0 || !strings.Contains(tokens[0], "cua-driver") {
					continue
				}
				var socket *string
				for i := 0; i < len(tokens)-1; i++ {
					if tokens[i] == "--socket" {
						s := doctorExpandUser(strings.ReplaceAll(tokens[i+1], "%h", userHome))
						socket = &s
						break
					}
				}
				serve := false
				for _, t := range tokens[1:] {
					serve = serve || t == "serve"
				}
				units = append(units, cuDaemonUnit{src.kind, name, strings.TrimLeft(tokens[0], "-+:!"), serve, socket})
			}
		}
	}
	return units
}

// Daemon units whose cua-driver Exec points into a pruned versioned release directory
func staleCuaExecReferences(configDir string) [][3]string {
	userHome := doctorUserHome()
	var out [][3]string
	for _, u := range cuaDaemonUnits(configDir) {
		resolved := doctorExpandUser(strings.ReplaceAll(u.execTarget, "%h", userHome))
		if _, err := os.Stat(resolved); strings.Contains(resolved, "/packages/releases/") && err != nil {
			out = append(out, [3]string{u.kind, u.unit, u.execTarget})
		}
	}
	return out
}

// Host platform as sys.platform names it
var doctorSysPlatform = func() string { return cuPlatform }

// Liveness check per configured serve daemon unit on Linux, degrading an ok overall for a silent socket
func applyDaemonLivenessGuard(env *tools.Env, report *pyjson.Object, binary string) *pyjson.Object {
	checks, isList := report.Vals["checks"].([]any)
	if doctorSysPlatform() != "linux" || !isList {
		return report
	}
	for _, u := range cuaDaemonUnits("") {
		if !u.runsServe {
			continue
		}
		sock := ""
		if u.socket != nil {
			sock = *u.socket
		}
		listening := cuaDaemonListening(env, binary, sock, 3)
		if listening == nil {
			continue
		}
		shown := "the default socket"
		if sock != "" {
			shown = sock
		}
		c := pyjson.NewObject()
		c.Set("name", "daemon ("+u.unit+")")
		if *listening {
			c.Set("status", "pass")
			c.Set("message", "cua-driver serve is listening on "+shown)
			checks = append(checks, c)
			continue
		}
		c.Set("status", "fail")
		c.Set("message", fmt.Sprintf(cuDeadDaemonMsg, u.unit, shown))
		c.Set("hint", fmt.Sprintf(cuDeadDaemonHint, u.unit, u.unit))
		checks = append(checks, c)
		if report.Vals["overall"] == "ok" {
			report.Set("overall", "degraded")
		}
	}
	report.Set("checks", checks)
	return report
}

// Fail check per daemon unit whose Exec target was pruned, degrading an ok overall
func applyStaleUnitGuard(report *pyjson.Object) *pyjson.Object {
	if doctorSysPlatform() != "linux" {
		return report
	}
	for _, f := range staleCuaExecReferences("") {
		checks, isList := report.Vals["checks"].([]any)
		if !isList {
			continue
		}
		c := pyjson.NewObject()
		c.Set("name", "daemon unit ("+f[1]+")")
		c.Set("status", "fail")
		c.Set("message", fmt.Sprintf(cuPrunedUnitMsg, f[2]))
		c.Set("hint", fmt.Sprintf(cuPrunedUnitHint, f[0]))
		report.Set("checks", append(checks, c))
		if report.Vals["overall"] == "ok" {
			report.Set("overall", "degraded")
		}
	}
	return report
}

// Environment note for Linux under Wayland, where doctor probes the CLI process and not the gateway
func waylandEnvironmentContext(report *pyjson.Object) *pyjson.Object {
	if report.Vals["platform"] != "linux" || os.Getenv("WAYLAND_DISPLAY") == "" {
		return nil
	}
	o := pyjson.NewObject()
	o.Set("scope", "cli_process")
	o.Set("gateway_environment_checked", false)
	return o
}

// Report value or the question mark default when the key is absent
func reportField(report *pyjson.Object, k string) any {
	if v, ok := report.Vals[k]; ok {
		return v
	}
	return "?"
}

// Renders the report like the driver does with identity, environment, checks, hints and data rows
func printTextReport(w io.Writer, report *pyjson.Object, color bool, identity, environment *pyjson.Object) {
	platform, reportV, overall := pyrepr.Str(reportField(report, "platform")), reportField(report, "driver_version"), reportField(report, "overall")
	overallS := pyrepr.Str(overall)
	if identity == nil {
		identity = pyjson.NewObject()
	}
	cliV := ""
	if v := identity.Vals["cli_version"]; pyrepr.Truthy(v) {
		cliV = pyrepr.Str(v)
	}
	headerV := cliV
	if headerV == "" {
		headerV = pyrepr.Str(reportV)
	}
	red, yellow, green, reset, dim := "", "", "", "", ""
	if _, known := cuOverallGlyph[overallS]; color && known {
		red, yellow, green, reset, dim = "\033[31m", "\033[33m", "\033[32m", "\033[0m", "\033[2m"
	}
	colFor := map[string]string{"failed": red, "degraded": yellow, "ok": green}[overallS]
	statusCols := map[string]string{"pass": green, "fail": red, "skip": dim}
	glyph, ok := cuOverallGlyph[overallS]
	if !ok {
		glyph = "•"
	}
	lines := []string{fmt.Sprintf("%s cua-driver %s on %s — %s%s%s", glyph, headerV, platform, colFor, overallS, reset)}
	if b := identity.Vals["resolved_binary"]; pyrepr.Truthy(b) {
		lines = append(lines, fmt.Sprintf("  %sbinary: %s%s", dim, pyrepr.Str(b), reset))
	}
	rv := pyrepr.Str(reportV)
	if cliV != "" && pyrepr.Truthy(reportV) && !strings.Contains(cliV, rv) && !strings.Contains(rv, cliV) {
		lines = append(lines, fmt.Sprintf("  %s--version: %s%s", dim, cliV, reset), fmt.Sprintf("  %shealth_report.driver_version: %s%s", dim, rv, reset))
	}
	if environment != nil && len(environment.Keys) > 0 {
		lines = append(lines, fmt.Sprintf("  %senvironment: current CLI process%s", dim, reset),
			fmt.Sprintf("  %sgateway environment was not checked; active gateway computer_use sessions use that process environment%s", dim, reset))
	}
	if pyrepr.Truthy(identity.Vals["version_mismatch"]) {
		lines = append(lines, fmt.Sprintf("  %s⚠️ version mismatch: health_report says %s but binary --version is %s%s", yellow, pyrepr.Repr(reportV), pyrepr.Repr(cliV), reset),
			fmt.Sprintf("  %s→ trust --version / packages/current for debugging; health_report's binary_version check can lag on Windows%s", dim, reset))
	}
	checks, _ := report.Vals["checks"].([]any)
	for _, raw := range checks {
		c, _ := raw.(*pyjson.Object)
		if c == nil {
			continue
		}
		status := pyrepr.Str(reportFieldOr(c, "status", "?"))
		g, ok := cuStatusGlyph[status]
		if !ok {
			g = "•"
		}
		msg := ""
		if m := c.Vals["message"]; pyrepr.Truthy(m) {
			msg = pyrepr.Str(m)
		}
		lines = append(lines, fmt.Sprintf("  %s %s%s%s: %s", g, statusCols[status], pyrepr.Str(reportFieldOr(c, "name", "?")), reset, msg))
		if h := c.Vals["hint"]; pyrepr.Truthy(h) {
			lines = append(lines, fmt.Sprintf("      → %s%s%s", dim, pyrepr.Str(h), reset))
		}
		if d, ok := c.Vals["data"].(*pyjson.Object); ok {
			for _, k := range d.Keys {
				v := d.Vals[k]
				s := pyrepr.Str(v)
				switch v.(type) {
				case *pyjson.Object, []any:
					s, _ = pyjson.Dumps(v)
				}
				lines = append(lines, fmt.Sprintf("      %s%s=%s%s", dim, k, s, reset))
			}
		}
	}
	fmt.Fprintln(w, strings.Join(lines, "\n"))
}

// Mapping value or the default when the key is absent
func reportFieldOr(o *pyjson.Object, k string, def any) any {
	if v, ok := o.Vals[k]; ok {
		return v
	}
	return def
}

// True when the writer is a terminal device
func writerIsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// Doctor of the resolved cua-driver, exit 0 when ok, 1 when degraded or failed, 2 when missing or broken
func runComputerUseDoctor(env *tools.Env, stdout, stderr io.Writer, driverCmd *string, include, skip []string, jsonOutput bool, color *bool) (int, error) {
	binary, err := resolveCuaDriverCmd(driverCmd)
	if err != nil {
		return 0, err
	}
	if binary == "" {
		shown := "cua-driver (PATH and canonical install paths)"
		if driverCmd != nil && *driverCmd != "" {
			shown = *driverCmd
		}
		fmt.Fprintf(stdout, "cua-driver: not installed (looked for %s).\n  Run: icebath computer-use install\n", pyrepr.Repr(shown))
		return 2, nil
	}
	report, err := driveHealthReport(env, binary, include, skip, 12)
	var unavailable *healthReportUnavailable
	if errors.As(err, &unavailable) {
		report, err = composeFallbackReport(env, binary, unavailable.msg, 12)
	}
	var spawn *spawnError
	var rt *doctorRuntimeError
	switch {
	case errors.As(err, &spawn):
		fmt.Fprintf(stderr, "cua-driver could not be started from %s: %v\n"+
			"  The Icebath runtime interpreter cannot execute this binary; the tool may still work because the\n"+
			"  shell resolves a different copy on PATH. Fix: install cua-driver outside the protected directory\n"+
			"  (e.g. the upstream installer's default under your user profile) or point ICEBATH_CUA_DRIVER_CMD at\n"+
			"  a copy the runtime can execute, then re-run `icebath computer-use doctor`.\n", pyrepr.Repr(binary), spawn)
		return 2, nil
	case errors.As(err, &rt):
		fmt.Fprintf(stderr, "cua-driver health_report failed: %v\n", rt)
		return 2, nil
	case err != nil:
		return 0, err
	}
	report = applyDisplayCountGuard(report)
	report = applyStaleTCCGuard(report)
	report = applyStaleUnitGuard(report)
	report = applyDaemonLivenessGuard(env, report, binary)
	identity := buildIdentity(env, binary, report)
	environment := waylandEnvironmentContext(report)
	if jsonOutput {
		payload := pyjson.NewObject()
		for _, k := range report.Keys {
			payload.Set(k, report.Vals[k])
		}
		payload.Set("icebath_identity", identity)
		if environment != nil {
			payload.Set("icebath_environment", environment)
		}
		text, err := pyjson.Dumps(payload, pyjson.Indent(2), pyjson.SortKeys(true))
		if err != nil {
			return 0, err
		}
		fmt.Fprint(stdout, text+"\n")
	} else {
		useColor := writerIsTTY(stdout)
		if color != nil {
			useColor = *color
		}
		printTextReport(stdout, report, useColor, identity, environment)
	}
	if report.Vals["overall"] == "ok" {
		return 0, nil
	}
	return 1, nil
}
