package browser

import (
	"context"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Raw secret value as get_secret returns it, whitespace kept
func (s settings) rawSecret(name string) string {
	if s.env != nil && s.env.Secrets != nil {
		return s.env.Secrets.String(name)
	}
	return os.Getenv(name)
}

// Pre CLI Browser Use cloud config, no other provider and no gateway with BROWSER_USE_API_KEY set
func isLegacyBrowserUseCloudConfig(env *tools.Env) bool {
	s := cfgOf(env)
	sel, _ := s.cloudProviderSelection()
	if (sel != "browser-use" && sel != "") || s.useGateway() {
		return false
	}
	return s.rawSecret("BROWSER_USE_API_KEY") != ""
}

const (
	execDefaultTimeoutS = 300
	execMaxTimeoutS     = 1800
)

const execHeaderBase = "Drive a real browser for JS pages, logins, forms and clicks (Browser Use CLI). `code` runs as full " +
	"Python (stdlib available) with pre-imported browser helpers; stdout comes back in the result. Start `code` with a one-line " +
	"comment describing the step for the user in plain language, max 60 chars " +
	"(e.g. `# Searching Amazon for paper towels`) \u2014 the UI shows it as the step label.\n\n" +
	"STATE: the browser session and workspace persist across calls; Python variables do NOT (fresh " +
	"interpreter each call). The workspace dir is $BH_AGENT_WORKSPACE (also `workspace` in every result); " +
	"functions defined in agent_helpers.py there are auto-imported into every call. For multi-item tasks " +
	"('all N products / every entry'), append each batch to a JSON/CSV file in the workspace, then read it " +
	"back and aggregate in code \u2014 dedupe/count/sort with Python, not in your head \u2014 and verify the " +
	"collected count against what was asked before answering.\n\n" +
	"Batch each sub-procedure (navigate, wait, extract, act) into one call \u2014 do not spend a call per " +
	"action \u2014 but for long extractions prefer several medium calls that append to workspace files over " +
	"one giant call, so progress survives timeouts."

const execHeaderVision = " Screenshots are attached to your context automatically: when the exec output contains a " +
	"capture_screenshot() path, the image arrives with this tool's result and you inspect it directly " +
	"with your own vision \u2014 never send browser screenshots to a separate vision tool."

const execTextFirst = "page_info() for state, js() for reading/extracting DOM text, fill_input(selector, text) for inputs, and " +
	"js(\"document.querySelector('\u2026').click()\") for clicks."

const execHeaderTextOnly = tools.ScreenshotsDetached + " Work text-first: " + execTextFirst

const execHeaderLightpanda = " The local engine is Lightpanda (no graphical renderer, one page per session): capture_screenshot() " +
	"is unavailable, so work text-first: " + execTextFirst + " Navigate with new_tab(url) exactly once, then " +
	"goto_url(url) for every later navigation \u2014 a second new_tab() fails with TargetAlreadyLoaded."

const execHelpersDigest = "\n\nHELPERS (pre-imported): new_tab(url) opens/navigates (use for the FIRST navigation), goto_url(url) " +
	"navigates the current tab, wait_for_load() after navigation, page_info() summarizes the current page " +
	"state, js(expr) evaluates a JS expression and returns its value (js('document.title'); wrap function " +
	"bodies as js('(() => {...})()') \u2014 a bare '() => {...}' returns the function itself, uncalled), " +
	"fill_input(selector, text) types into inputs, click_at_xy(x, y) clicks viewport coordinates, " +
	"capture_screenshot() saves and prints a screenshot path, cdp('Domain.method', **kwargs) is raw CDP \u2014 " +
	"cdp('Accessibility.getFullAXTree')['nodes'] lists every element's role/name/backendDOMNodeId (filter " +
	"in Python before printing; it is thousands of nodes), then cdp('DOM.getBoxModel', backendNodeId=n) " +
	"gives click coordinates. ensure_real_tab() recovers from a stale/internal tab. Login walls: never guess " +
	"credentials; see the vault note below if present, otherwise stop and ask the user."

const execLocalDescription = "Drive the user's own local browser (an Icebath-managed copy of their real " +
	"default-Chromium profile, logins/cookies included) instead of the configured " +
	"cloud browser backend. Use when the user asks to act as themselves \u2014 their " +
	"accounts, their sessions. No-op when the backend is already local. Default false."

// Static browser_exec schema, the description used only while the managed CLI is unavailable
func BrowserExecSchema() map[string]any {
	return map[string]any{
		"name": "browser_exec",
		"description": execHeaderBase + execHelpersDigest +
			"\n\n(The browser-use CLI is not installed yet. Install it with `icebath tools` (Browser Automation \u2192 Browser Use).)",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code":    map[string]any{"type": "string", "description": "Python code to execute using the pre-imported browser helpers. Use print(...) for any data you need back."},
				"session": map[string]any{"type": "string", "description": "Named isolated browser session \u2014 its own daemon and (on cloud backends) own browser, so concurrent tasks don't share tabs. Reuse the same name on every related call; omit for the shared default session."},
				"timeout_s": map[string]any{"type": "integer", "default": execDefaultTimeoutS,
					"description": fmt.Sprintf("Max seconds to wait for the code to finish (default %d, max %d).", execDefaultTimeoutS, execMaxTimeoutS)},
			},
			"required": []any{"code"},
		},
	}
}

// Real profile consent of the active profile, read on every call
func realProfileConsented(env *tools.Env) bool { return cfgOf(env).useRealProfile() }

// Description header by whether screenshots ride back natively, lightpanda first since it renders none
func execDescriptionHeader(env *tools.Env) string {
	if on, _ := lightpandaEngineStatus(env); on {
		return execHeaderBase + execHeaderLightpanda
	}
	if tools.ShouldUseNativeVisionFastPath(context.Background(), env) {
		return execHeaderBase + execHeaderVision
	}
	return execHeaderBase + execHeaderTextOnly
}

// Session tailored description, plus the local parameter only when real profile browsing is consented
func BrowserExecSchemaOverrides(env *tools.Env) map[string]any {
	overrides := map[string]any{"description": execDescriptionHeader(env) + execHelpersDigest}
	if realProfileConsented(env) {
		static := BrowserExecSchema()["parameters"].(map[string]any)
		props := map[string]any{}
		for k, v := range static["properties"].(map[string]any) {
			props[k] = v
		}
		props["local"] = map[string]any{"type": "boolean", "default": false, "description": execLocalDescription}
		params := map[string]any{}
		for k, v := range static {
			params[k] = v
		}
		params["properties"] = props
		overrides["parameters"] = params
	}
	return overrides
}

// Browser harness importable, never launching or opening sockets
var browserExecAvailable = func(env *tools.Env) bool { return findCLI(env) != nil }

// Runs browser_exec counted against the backend that served it, a raised error reaching the dispatcher
func handleBrowserExec(ctx context.Context, call *tools.Call) string {
	args := call.Args
	code, ok := args["code"]
	if !ok {
		code = ""
	}
	session := args["session"]
	if !pyrepr.Truthy(session) {
		session = ""
	}
	timeout, ok := args["timeout_s"]
	if !ok {
		timeout = execDefaultTimeoutS
	}
	local := pyrepr.Truthy(args["local"])
	localEngine := ""
	out, err := observability.RecordBrowserCall(ctx, call.Env, func() (any, error) {
		return browserExec(ctx, call.Env, code, session, timeout, call.TaskID, local, &localEngine)
	}, func() (string, error) { return browserBackendName(call.Env, localEngine) })
	if err != nil {
		panic(err)
	}
	return out.(string)
}

func registerBrowserExecTool() {
	tools.Register(tools.Def{Name: "browser_exec", Toolset: "browser-use", Schema: BrowserExecSchema(), Handler: handleBrowserExec,
		Check: browserExecAvailable, DynamicSchema: BrowserExecSchemaOverrides, Emoji: "\U0001F310"})
}

func init() { provider.BrowserHarnessCLI = findCLI }
