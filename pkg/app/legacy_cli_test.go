package app

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/pm"
)

const legacyAgentUsage = `usage: icebath-agent [-h] [--version] [--query QUERY] [--model MODEL]
                     [--api-key API_KEY] [--base-url BASE_URL]
                     [--max-turns MAX_TURNS]
                     [--enabled-toolsets ENABLED_TOOLSETS]
                     [--disabled-toolsets DISABLED_TOOLSETS] [--list-tools]
                     [--save-trajectories] [--save-sample] [--verbose]
                     [--log-prefix-chars LOG_PREFIX_CHARS]
                     [prompt ...]
`

const legacyAgentHelp = legacyAgentUsage + `
Legacy single-query Icebath Agent runner. For the full CLI use ` + "`icebath`." + `

positional arguments:
  prompt                query to run (same as --query)

options:
  -h, --help            show this help message and exit
  --version             show program's version number and exit
  --query, -q QUERY     natural-language query to run
  --model MODEL         model id (provider/model)
  --api-key, --api_key API_KEY
                        API key for the model endpoint
  --base-url, --base_url BASE_URL
                        model API base URL
  --max-turns, --max_turns MAX_TURNS
                        maximum API call iterations (default: 10)
  --enabled-toolsets, --enabled_toolsets ENABLED_TOOLSETS
                        comma-separated toolsets to enable
  --disabled-toolsets, --disabled_toolsets DISABLED_TOOLSETS
                        comma-separated toolsets to disable
  --list-tools, --list_tools
                        list available tools and exit
  --save-trajectories, --save_trajectories
                        append the conversation to trajectory JSONL files
  --save-sample, --save_sample
                        save one trajectory sample to a UUID-named file
  --verbose             verbose logging
  --log-prefix-chars, --log_prefix_chars LOG_PREFIX_CHARS
                        characters shown in tool-call log previews (default:
                        20)
`

func runLegacy(t *testing.T, argv ...string) (int, string, string, []agent.MainOptions) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("COLUMNS", "80")
	var stdout, stderr bytes.Buffer
	var calls []agent.MainOptions
	code := LegacyAgentMain(context.Background(), argv, &stdout, &stderr, func(_ context.Context, _ io.Writer, o agent.MainOptions) {
		calls = append(calls, o)
	})
	return code, stdout.String(), stderr.String(), calls
}

func TestLegacyAgentHelpAndVersion(t *testing.T) {
	for _, argv := range [][]string{{"--help"}, {"-h", "--max-turns", "x"}, {"-he"}, {"--foo", "-h"}} {
		code, out, errText, calls := runLegacy(t, argv...)
		if code != 0 || out != legacyAgentHelp || errText != "" || calls != nil {
			t.Fatalf("%q: code %d calls %v stderr %q stdout\n%s", argv, code, calls, errText, out)
		}
	}
	for _, argv := range [][]string{{"--version"}, {"--version", "--foo"}, {"--foo", "--version"}} {
		code, out, _, calls := runLegacy(t, argv...)
		if want := "Icebath Agent v" + pm.DunderVersion() + " (2026.9.24)\n"; code != 0 || out != want || calls != nil {
			t.Fatalf("%q: code %d stdout %q want %q", argv, code, out, want)
		}
	}
}

func TestLegacyAgentParseErrors(t *testing.T) {
	cases := []struct {
		argv []string
		msg  string
	}{
		{[]string{"a", "--verbose", "b"}, "unrecognized arguments: b"},
		{[]string{"--api", "x"}, "ambiguous option: --api could match --api-key, --api_key"},
		{[]string{"--max-turns", "x"}, "argument --max-turns/--max_turns: invalid int value: 'x'"},
		{[]string{"--max-turns", "x", "-h"}, "argument --max-turns/--max_turns: invalid int value: 'x'"},
		{[]string{"--foo", "--bar", "z"}, "unrecognized arguments: --foo --bar"},
		{[]string{"-q"}, "argument --query/-q: expected one argument"},
		{[]string{"--save", "x"}, "ambiguous option: --save could match --save-trajectories, --save_trajectories, --save-sample, --save_sample"},
		{[]string{"--verbose=1"}, "argument --verbose: ignored explicit argument '1'"},
		{[]string{"--list-tools", "--list-tools=1"}, "argument --list-tools/--list_tools: ignored explicit argument '1'"},
		{[]string{"--model"}, "argument --model: expected one argument"},
		{[]string{"-x"}, "unrecognized arguments: -x"},
		{[]string{"--query", "-x"}, "argument --query/-q: expected one argument"},
		{[]string{"-q", "--"}, "argument --query/-q: expected one argument"},
		{[]string{"--ver"}, "ambiguous option: --ver could match --version, --verbose"},
		{[]string{"-vq"}, "unrecognized arguments: -vq"},
		{[]string{"a", "-x", "b"}, "unrecognized arguments: -x b"},
		{[]string{"--query", "a b", "-x"}, "unrecognized arguments: -x"},
		{[]string{"--max-turns", "0x10"}, "argument --max-turns/--max_turns: invalid int value: '0x10'"},
		{[]string{"--query", "q", "word"}, "pass the query either positionally or via --query, not both"},
	}
	for _, c := range cases {
		code, out, errText, calls := runLegacy(t, c.argv...)
		if want := legacyAgentUsage + "icebath-agent: error: " + c.msg + "\n"; code != 2 || errText != want || out != "" || calls != nil {
			t.Fatalf("%q: code %d stdout %q stderr\n%s\nwant\n%s", c.argv, code, out, errText, want)
		}
	}
}

func TestLegacyAgentRunsOneQuery(t *testing.T) {
	str := func(s string) *string { return &s }
	base := agent.MainOptions{MaxTurns: 10, LogPrefixChars: 20}
	with := func(f func(*agent.MainOptions)) agent.MainOptions { o := base; f(&o); return o }
	cases := []struct {
		argv []string
		want agent.MainOptions
	}{
		{[]string{"--que", "q"}, with(func(o *agent.MainOptions) { o.Query = str("q") })},
		{[]string{"-qhello"}, with(func(o *agent.MainOptions) { o.Query = str("hello") })},
		{[]string{"-q=x"}, with(func(o *agent.MainOptions) { o.Query = str("x") })},
		{[]string{"--verbose", "a", "b"}, with(func(o *agent.MainOptions) { o.Query, o.Verbose = str("a b"), true })},
		{[]string{"a", "b", "--verbose"}, with(func(o *agent.MainOptions) { o.Query, o.Verbose = str("a b"), true })},
		{[]string{"--", "-x", "y"}, with(func(o *agent.MainOptions) { o.Query = str("-x y") })},
		{[]string{"a", "--", "--verbose"}, with(func(o *agent.MainOptions) { o.Query = str("a --verbose") })},
		{[]string{"--", "a", "--", "b"}, with(func(o *agent.MainOptions) { o.Query = str("a -- b") })},
		{[]string{"-5"}, with(func(o *agent.MainOptions) { o.Query = str("-5") })},
		{[]string{"-"}, with(func(o *agent.MainOptions) { o.Query = str("-") })},
		{[]string{"--foo bar"}, with(func(o *agent.MainOptions) { o.Query = str("--foo bar") })},
		{[]string{"--query", "-5"}, with(func(o *agent.MainOptions) { o.Query = str("-5") })},
		{[]string{"--query=", "  spaced  "}, with(func(o *agent.MainOptions) { o.Query = str("spaced") })},
		{[]string{"--query", "a", "--query", "b"}, with(func(o *agent.MainOptions) { o.Query = str("b") })},
		{[]string{"q", "--max_t", "4", "--max-turns", " 7 "}, with(func(o *agent.MainOptions) { o.Query, o.MaxTurns = str("q"), 7 })},
		{[]string{"q", "--max-turns", "-3", "--log-prefix-chars", "1_000"}, with(func(o *agent.MainOptions) { o.Query, o.MaxTurns, o.LogPrefixChars = str("q"), -3, 1000 })},
		{[]string{"q", "--max-turns", "\u0663"}, with(func(o *agent.MainOptions) { o.Query, o.MaxTurns = str("q"), 3 })},
		{[]string{"--list-tools"}, with(func(o *agent.MainOptions) { o.ListTools = true })},
		{[]string{"--list_tools", "--model", "m/x", "--api_key", "k", "--base_url", "u", "--enabled_toolsets", "web", "--disabled-toolsets", "terminal", "--save_trajectories", "--save-sample"},
			with(func(o *agent.MainOptions) {
				o.ListTools, o.Model, o.APIKey, o.BaseURL, o.EnabledToolsets, o.DisabledToolsets, o.SaveTrajectories, o.SaveSample = true, "m/x", "k", "u", "web", "terminal", true, true
			})},
	}
	for _, c := range cases {
		code, out, errText, calls := runLegacy(t, c.argv...)
		if code != 0 || out != "" || errText != "" || len(calls) != 1 || !reflect.DeepEqual(calls[0], c.want) {
			t.Fatalf("%q: code %d stdout %q stderr %q calls %+v want %+v", c.argv, code, out, errText, calls, c.want)
		}
	}
}

func TestLegacyAgentBareInvocationPrintsHelp(t *testing.T) {
	for _, argv := range [][]string{nil, {"--query="}, {"  "}, {"--verbose"}} {
		code, out, errText, calls := runLegacy(t, argv...)
		want := legacyAgentHelp + "\nNo query given: pass one with --query (or run `icebath` for the interactive CLI).\n"
		if code != 0 || out != want || errText != "" || calls != nil {
			t.Fatalf("%q: code %d stderr %q calls %v stdout\n%s", argv, code, errText, calls, out)
		}
	}
}

func TestLegacyAgentArgvDispatch(t *testing.T) {
	cases := []struct {
		args []string
		want []string
		ok   bool
	}{
		{[]string{"/x/icebath", pm.LegacyAgentEntry, "-q", "hi"}, []string{"-q", "hi"}, true},
		{[]string{"/home/u/.local/bin/icebath-agent", "--version"}, []string{"--version"}, true},
		{[]string{"/c/bin/icebath-agent.exe"}, []string{}, true},
		{[]string{"/x/icebath", "chat"}, nil, false},
		{nil, nil, false},
	}
	for _, c := range cases {
		got, ok := LegacyAgentArgv(c.args)
		if ok != c.ok || (ok && !reflect.DeepEqual(got, c.want)) || (!ok && got != nil) {
			t.Fatalf("%q: got %q %v", c.args, got, ok)
		}
	}
	if strings.Contains(legacyAgentHelp, "hermes") {
		t.Fatal("help names the upstream command")
	}
}
