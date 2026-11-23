package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
)

const doctorLiveDefaultProbeTimeout = 10.0

// One metadata endpoint probe, its URL, the variable holding its key and the auth scheme
type doctorLiveKeyedSpec struct {
	Name, URL, EnvVar, Scheme string
}

// Metadata only endpoints of the keyed tool backends in probe order
var doctorLiveKeyedProbes = []doctorLiveKeyedSpec{
	{"Firecrawl", "https://api.firecrawl.dev/v2/team/credit-usage", "FIRECRAWL_API_KEY", "Bearer"},
	{"FAL", "https://fal.ai/api/models?page=1", "FAL_KEY", "Key"},
}

// TTS and STT providers that never touch the network
var doctorLiveLocalAudioProviders = map[string]bool{"": true, "local": true, "edge": true, "neutts": true, "kittentts": true, "piper": true}

// Metadata endpoints of the remote TTS and STT providers
var doctorLiveAudioProbes = map[string]doctorLiveKeyedSpec{
	"openai":     {URL: "https://api.openai.com/v1/models", EnvVar: "OPENAI_API_KEY", Scheme: "Bearer"},
	"groq":       {URL: "https://api.groq.com/openai/v1/models", EnvVar: "GROQ_API_KEY", Scheme: "Bearer"},
	"elevenlabs": {URL: "https://api.elevenlabs.io/v1/voices", EnvVar: "ELEVENLABS_API_KEY", Scheme: "xi"},
}

// Outcome of one live backend probe, the status being pass, warn, fail or skip
type doctorLiveProbeResult struct {
	Name, Status, Detail string
}

// Seams the live probes of one doctor run reach the config, the network, the browser and MCP servers through
type doctorLiveEnv struct {
	ctx                context.Context
	d                  *Doctor
	loadConfig         func() (map[string]any, error)
	httpGet            func(target string, headers [][2]string, timeout float64) (int, error)
	browserAvailable   func() bool
	launchBrowserProbe func(timeout float64) (bool, string)
	probeMCPServer     func(name string, cfg map[string]any, timeout float64) (int, error)
	speak              func(text, outputPath string) (string, error)
	transcribe         func(path string) (map[string]any, error)
	sample             func() (string, string, error)
	spoken             string
}

// Live probe seams of a doctor run bound to the real config, network and MCP client, plus the browser and speech of linked tiers
func newDoctorLiveEnv(ctx context.Context, d *Doctor) *doctorLiveEnv {
	e := &doctorLiveEnv{ctx: ctx, d: d}
	e.loadConfig = func() (map[string]any, error) {
		cfg, err := config.LoadReadonly(ctx, d.Home, nil)
		return map[string]any(cfg), err
	}
	e.httpGet = e.doctorLiveHTTPGet
	e.probeMCPServer = e.doctorProbeMCPServer
	if doctorLiveBrowser != nil {
		doctorLiveBrowser(e)
	}
	if doctorLiveSpeech != nil {
		doctorLiveSpeech(e)
	}
	return e
}

// Binders of the live browser and local speech probes, set by their tiers and nil in a build without them
var (
	doctorLiveBrowser func(e *doctorLiveEnv)
	doctorLiveSpeech  func(e *doctorLiveEnv)
)

// Sentence the local speech probe speaks and the transcription probe expects to hear back
const doctorLiveSentence = "The quick brown fox jumps over the lazy dog."

// Speaks the check sentence through the configured local voice, keeping the audio for the transcription probe
func (e *doctorLiveEnv) doctorLiveSpeak(name, provider string) (doctorLiveProbeResult, error) {
	dir, err := os.MkdirTemp("", "icebath-doctor-speech-")
	if err != nil {
		return doctorLiveProbeResult{}, err
	}
	reply, err := e.speak(doctorLiveSentence, filepath.Join(dir, "check.ogg"))
	if err != nil {
		os.RemoveAll(dir)
		return doctorLiveProbeResult{name, "fail", "(" + provider + ") " + err.Error()}, nil
	}
	var result struct {
		Success  bool   `json:"success"`
		FilePath string `json:"file_path"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal([]byte(reply), &result); err != nil || !result.Success {
		os.RemoveAll(dir)
		return doctorLiveProbeResult{name, "fail", "(" + provider + ") " + result.Error}, nil
	}
	info, err := os.Stat(result.FilePath)
	if err != nil {
		os.RemoveAll(dir)
		return doctorLiveProbeResult{name, "fail", "(" + provider + ") " + err.Error()}, nil
	}
	e.spoken = result.FilePath
	return doctorLiveProbeResult{name, "pass", fmt.Sprintf("(%s spoke %q into %s, %d KB)", provider, doctorLiveSentence, filepath.Base(result.FilePath), info.Size()/1024)}, nil
}

// Words of a sentence lower cased without punctuation
func doctorLiveWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// Transcribes the spoken check sentence, or the bundled voice sample without one, passing when most expected words come back
func (e *doctorLiveEnv) doctorLiveHear(name, provider string) (doctorLiveProbeResult, error) {
	audio, expected := e.spoken, doctorLiveSentence
	if audio != "" {
		defer os.RemoveAll(filepath.Dir(audio))
		e.spoken = ""
	} else {
		var err error
		if audio, expected, err = e.sample(); err != nil {
			return doctorLiveProbeResult{}, err
		}
	}
	reply, err := e.transcribe(audio)
	if err != nil {
		return doctorLiveProbeResult{name, "fail", "(" + provider + ") " + err.Error()}, nil
	}
	transcript := pyrepr.Str(reply["transcript"])
	if !pyrepr.Truthy(reply["success"]) {
		return doctorLiveProbeResult{name, "fail", "(" + provider + ") " + pyrepr.Str(reply["error"])}, nil
	}
	heard := map[string]bool{}
	for _, word := range doctorLiveWords(transcript) {
		heard[word] = true
	}
	want := doctorLiveWords(expected)
	found := 0
	for _, word := range want {
		if heard[word] {
			found++
		}
	}
	detail := fmt.Sprintf("(%s heard %q)", provider, strings.TrimSpace(transcript))
	if 2*found < len(want) {
		return doctorLiveProbeResult{name, "fail", detail[:len(detail)-1] + fmt.Sprintf(", expected %q)", expected)}, nil
	}
	return doctorLiveProbeResult{name, "pass", detail}, nil
}

// Tool environment of the doctor home carrying the read only config
func (e *doctorLiveEnv) toolsEnv() *tools.Env {
	cfg, err := config.LoadReadonly(e.ctx, e.d.Home, nil)
	if err != nil {
		return &tools.Env{Home: e.d.Home}
	}
	return &tools.Env{Home: e.d.Home, Config: cfg}
}

// Sends one metadata GET after sanitizing the proxy environment and returns the response status
func (e *doctorLiveEnv) doctorLiveHTTPGet(target string, headers [][2]string, timeout float64) (int, error) {
	config.NormalizeProxyEnvVars()
	req, err := http.NewRequestWithContext(e.ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	resp, err := pyhttpx.Do(pyhttpx.EnvProxyClient(false), req, time.Duration(timeout*float64(time.Second)))
	if err != nil {
		return 0, errors.New(pyhttpx.ErrorText(err))
	}
	return resp.StatusCode, nil
}

// Connects one configured MCP server, lists its tools and returns how many it offers
func (e *doctorLiveEnv) doctorProbeMCPServer(name string, cfg map[string]any, timeout float64) (int, error) {
	found, err := mcp.ProbeSingleServerTimeout(e.ctx, e.toolsEnv(), name, cfg, timeout)
	return len(found), err
}

// Classifies a metadata response status, naming the key variable on an auth failure
func doctorLiveClassifyHTTP(name string, code int, keyHint string) doctorLiveProbeResult {
	if code >= 200 && code < 300 {
		return doctorLiveProbeResult{name, "pass", fmt.Sprintf("(HTTP %d)", code)}
	}
	if code == 401 || code == 403 {
		return doctorLiveProbeResult{name, "fail", fmt.Sprintf("(HTTP %d — check %s)", code, keyHint)}
	}
	return doctorLiveProbeResult{name, "fail", fmt.Sprintf("(HTTP %d)", code)}
}

// Probes one metadata endpoint authenticated by a single env var, skipped when the var is unset
func (e *doctorLiveEnv) doctorLiveKeyedProbe(spec doctorLiveKeyedSpec, timeout float64) (doctorLiveProbeResult, error) {
	key := pystr.Strip(os.Getenv(spec.EnvVar))
	if key == "" {
		return doctorLiveProbeResult{spec.Name, "skip", "(not configured)"}, nil
	}
	code, err := e.httpGet(spec.URL, [][2]string{{"Authorization", spec.Scheme + " " + key}}, timeout)
	if err != nil {
		return doctorLiveProbeResult{}, err
	}
	return doctorLiveClassifyHTTP(spec.Name, code, spec.EnvVar), nil
}

// Launches and stops the local browser when one is available
func (e *doctorLiveEnv) doctorLiveProbeBrowser(timeout float64) (doctorLiveProbeResult, error) {
	if e.browserAvailable == nil {
		line := linktier.Missing("browser automation", linktier.Extras)
		return doctorLiveProbeResult{"Browser", "skip", "(" + line + ")"}, nil
	}
	if !e.browserAvailable() {
		return doctorLiveProbeResult{"Browser", "skip", "(not configured)"}, nil
	}
	ok, detail := e.launchBrowserProbe(timeout)
	status := "fail"
	if ok {
		status = "pass"
	}
	return doctorLiveProbeResult{"Browser", status, "(" + detail + ")"}, nil
}

// Probes the voices or models list of the configured TTS or STT provider
func (e *doctorLiveEnv) doctorLiveProbeAudio(kind string, cfg map[string]any, timeout float64) (doctorLiveProbeResult, error) {
	name := pystr.Upper(kind)
	providerName := ""
	if section := cfg[kind]; pyrepr.Truthy(section) {
		block, ok := section.(map[string]any)
		if !ok {
			return doctorLiveProbeResult{}, fmt.Errorf("config %s section is %s where a mapping was expected", kind, errchain.Kind(section))
		}
		if raw := block["provider"]; pyrepr.Truthy(raw) {
			text, isStr := raw.(string)
			if !isStr {
				return doctorLiveProbeResult{}, fmt.Errorf("config %s provider is %s where text was expected", kind, errchain.Kind(raw))
			}
			providerName = pystr.Lower(pystr.Strip(text))
		}
	}
	if doctorLiveLocalAudioProviders[providerName] {
		if e.speak == nil {
			line := linktier.Missing("local speech", linktier.Plugins)
			return doctorLiveProbeResult{name, "skip", "(" + line + ")"}, nil
		}
		shown := providerName
		if shown == "" {
			shown = "local"
		}
		if kind == "tts" {
			return e.doctorLiveSpeak(name, shown)
		}
		return e.doctorLiveHear(name, shown)
	}
	spec, known := doctorLiveAudioProbes[providerName]
	if !known {
		return doctorLiveProbeResult{name, "skip", "(provider '" + providerName + "' — no live probe implemented)"}, nil
	}
	key := pystr.Strip(os.Getenv(spec.EnvVar))
	if key == "" {
		return doctorLiveProbeResult{name, "warn", "(provider '" + providerName + "' configured but " + spec.EnvVar + " is not set)"}, nil
	}
	headers := [][2]string{{"Authorization", "Bearer " + key}}
	if spec.Scheme == "xi" {
		headers = [][2]string{{"xi-api-key", key}}
	}
	code, err := e.httpGet(spec.URL, headers, timeout)
	if err != nil {
		return doctorLiveProbeResult{}, err
	}
	result := doctorLiveClassifyHTTP(name, code, spec.EnvVar)
	result.Detail = "(" + providerName + ") " + result.Detail
	return result, nil
}

// Prints the row of one live probe result and records an issue for a failed one
func (e *doctorLiveEnv) doctorLiveReport(result doctorLiveProbeResult, issues *[]string) {
	switch result.Status {
	case "pass":
		doctorCheckOK(e.d.W, result.Name, result.Detail)
	case "warn":
		doctorCheckWarn(e.d.W, result.Name, result.Detail)
	case "fail":
		doctorCheckFail(e.d.W, result.Name, result.Detail)
		*issues = append(*issues, "Live probe failed: "+result.Name+" "+result.Detail)
	default:
		doctorCheckInfo(e.d.W, result.Name+" "+result.Detail+" — skipped")
	}
}

// Reports whether an error is one Python raises as the builtin TimeoutError
func doctorLiveIsTimeout(err error) bool {
	var timeout *pyhttpx.TimeoutError
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, syscall.ETIMEDOUT) ||
		errors.As(err, &timeout)
}

// Runs one probe and reports it, a probe error becoming a fail row instead of ending the doctor run
func (e *doctorLiveEnv) doctorLiveRunOne(name string, fn func() (doctorLiveProbeResult, error), issues *[]string) doctorLiveProbeResult {
	result, err := fn()
	if err != nil {
		msg := err.Error()
		switch {
		case doctorLiveIsTimeout(err):
			result = doctorLiveProbeResult{name, "fail", "(timed out: " + msg + ")"}
		default:
			if msg == "" {
				t := reflect.TypeOf(err)
				for t.Kind() == reflect.Pointer {
					t = t.Elem()
				}
				msg = t.Name()
			}
			if strings.Contains(pystr.Lower(msg), "time") {
				result = doctorLiveProbeResult{name, "fail", "(timed out: " + msg + ")"}
			} else {
				result = doctorLiveProbeResult{name, "fail", "(" + msg + ")"}
			}
		}
	}
	e.doctorLiveReport(result, issues)
	return result
}

// Probe timeout in seconds of the doctor's home, at least one second
func doctorLiveTimeout(ctx context.Context, h string) float64 {
	return max(1.0, settings.DoctorLiveProbeTimeout.Get(home.WithOverride(ctx, h)).Seconds())
}

// Runs one bounded read only probe per configured tool backend in order, appending an issue per failed probe
func (e *doctorLiveEnv) doctorRunLiveChecks(issues *[]string) ([]doctorLiveProbeResult, error) {
	cfg, err := e.loadConfig()
	if err != nil {
		return nil, err
	}
	timeout := doctorLiveTimeout(e.ctx, e.d.Home)
	doctorSection(e.d.W, "Live Backend Probes (opt-in, real calls)")
	var results []doctorLiveProbeResult
	for _, spec := range doctorLiveKeyedProbes {
		results = append(results, e.doctorLiveRunOne(spec.Name, func() (doctorLiveProbeResult, error) {
			return e.doctorLiveKeyedProbe(spec, timeout)
		}, issues))
	}
	results = append(results, e.doctorLiveRunOne("Browser", func() (doctorLiveProbeResult, error) {
		return e.doctorLiveProbeBrowser(timeout)
	}, issues))
	servers, _ := cfg["mcp_servers"].(map[string]any)
	if len(servers) > 0 {
		names := make([]string, 0, len(servers))
		for name := range servers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			results = append(results, e.doctorLiveRunOne("MCP: "+name, func() (doctorLiveProbeResult, error) {
				entry, ok := servers[name].(map[string]any)
				if !ok {
					return doctorLiveProbeResult{"MCP: " + name, "skip", "(malformed config entry)"}, nil
				}
				n, perr := e.probeMCPServer(name, entry, timeout)
				if perr != nil {
					return doctorLiveProbeResult{}, perr
				}
				return doctorLiveProbeResult{"MCP: " + name, "pass", fmt.Sprintf("(%d tool(s))", n)}, nil
			}, issues))
		}
	} else {
		results = append(results, doctorLiveProbeResult{"MCP", "skip", "(no servers configured)"})
		e.doctorLiveReport(results[len(results)-1], issues)
	}
	for _, kind := range []string{"tts", "stt"} {
		results = append(results, e.doctorLiveRunOne(pystr.Upper(kind), func() (doctorLiveProbeResult, error) {
			return e.doctorLiveProbeAudio(kind, cfg, timeout)
		}, issues))
	}
	return results, nil
}

// Runs the live probes only when the live flag is set, printing a failed run as a warning
func (e *doctorLiveEnv) doctorMaybeRunLiveChecks(ns map[string]any, issues *[]string) []doctorLiveProbeResult {
	if !pyrepr.Truthy(ns["live"]) {
		return nil
	}
	results, err := e.doctorRunLiveChecks(issues)
	if err != nil {
		doctorCheckWarn(e.d.W, "Live backend probes crashed", "("+err.Error()+")")
		return nil
	}
	return results
}
