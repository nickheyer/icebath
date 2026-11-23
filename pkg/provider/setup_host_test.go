package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestHostedLineOffersTypedMenusAsLists(t *testing.T) {
	trail := &hostedOutput{w: io.Discard}
	fmt.Fprint(trail, "  Options:\n    1. Login with GitHub (OAuth device code flow)\n    2. Enter a token manually\n    3. Cancel\n\n")
	q := hostedLine(trail, "  Choice [1-3]: ", false)
	if q.Kind != HostedChoice || !reflect.DeepEqual(q.Rows, []string{"Login with GitHub (OAuth device code flow)", "Enter a token manually", "Cancel"}) ||
		!reflect.DeepEqual(q.Answers, []string{"1", "2", "3"}) {
		t.Fatalf("numbered menu %+v", q)
	}
	fmt.Fprint(trail, "Authentication:\n  1. API key                  (AZURE_FOUNDRY_API_KEY in .env)\n  2. Microsoft Entra ID       (managed identity)\n")
	q = hostedLine(trail, "Authentication mode [1/2] (2): ", false)
	if q.Kind != HostedChoice || q.Title != "Authentication mode" || q.Default != 1 || len(q.Rows) != 2 || q.Answers[1] != "2" {
		t.Fatalf("numbered menu with a default %+v", q)
	}
	fmt.Fprint(trail, "  Available models:\n    1. a\n    2. b\n")
	if q = hostedLine(trail, "  Select model [1-2] or type name: ", false); q.Kind != HostedText {
		t.Fatalf("a menu that takes a typed name must stay a line %+v", q)
	}
	q = hostedLine(trail, "Switch anyway? [y/N]: ", false)
	if q.Kind != HostedChoice || q.Title != "Switch anyway?" || q.Default != 1 || !reflect.DeepEqual(q.Answers, []string{"y", "n"}) {
		t.Fatalf("yes no %+v", q)
	}
	if q = hostedLine(trail, "  Add /v1? [Y/n]: ", false); q.Default != 0 {
		t.Fatalf("yes default %+v", q)
	}
	if q = hostedLine(trail, "Base URL [https://api.deepseek.com/v1]: ", false); q.Kind != HostedText || q.Title != "Base URL [https://api.deepseek.com/v1]:" {
		t.Fatalf("text %+v", q)
	}
	if q = hostedLine(trail, "  Token (COPILOT_GITHUB_TOKEN): ", true); q.Kind != HostedSecret {
		t.Fatalf("secret %+v", q)
	}
	answer, err := askHostedLine(func(HostedPrompt) HostedAnswer { return HostedAnswer{Index: 1} }, trail, "  Choice [1-3]: ", false)
	if err != nil || answer != "2" {
		t.Fatalf("menu answer %q %v", answer, err)
	}
	if _, err := askHostedLine(func(HostedPrompt) HostedAnswer { return HostedAnswer{Cancelled: true} }, trail, "Base URL: ", false); err == nil {
		t.Fatal("a backed out line must read as an interrupt")
	}
}

func TestHostedOutputNamesTheTUICommands(t *testing.T) {
	var out bytes.Buffer
	trail := &hostedOutput{w: &out}
	fmt.Fprint(trail, "Sign-in did not complete; run `icebath model` to try again.\nRun `icebath auth add copilot` to sign in.\n")
	fmt.Fprint(trail, "  API key cleared.  Re-run `icebath setup` to configure X again.\nRun `icebath setup tts` next. Partial")
	trail.flush()
	want := "Sign-in did not complete; run `/setup model` to try again.\nRun `/setup model copilot` to sign in.\n" +
		"  API key cleared.  Re-run `/setup` to configure X again.\nRun `/setup voice` next. Partial\n"
	if out.String() != want {
		t.Fatalf("wording %q", out.String())
	}
}

func TestHostSetupFlowPutsEveryPromptToTheHostAndRestoresTheTerminal(t *testing.T) {
	seams := []any{flowLineInputFn, flowRawInputFn, flowSecretInputFn, flowCursesChoiceFn, flowRadiolistFn, pickerRadiolistFn, pickerInputFn,
		pickerLineInputFn, setupRadiolist, setupChecklist, secretFieldFn, secretGetpass, oauthPromptFn, flowStdinIsTTYFn,
		pickerConsoleFn, anthRunOAuthSetupTokenFn, authCmdInputFn, authCmdLineInputFn, authCmdMaskedSecretPromptFn}
	pointers := func() []uintptr {
		out := make([]uintptr, len(seams))
		for i, s := range seams {
			out[i] = reflect.ValueOf(s).Pointer()
		}
		return out
	}
	before := pointers()
	var asked []HostedPrompt
	ask := func(p HostedPrompt) HostedAnswer {
		asked = append(asked, p)
		switch p.Kind {
		case HostedChoice:
			return HostedAnswer{Index: 1}
		case HostedChecklist:
			return HostedAnswer{Picked: []int{2, 0}}
		case HostedCommand:
			return HostedAnswer{Ran: true}
		}
		return HostedAnswer{Text: "typed " + p.Kind}
	}
	var out bytes.Buffer
	err := HostSetupFlow(ask, &out, func() error {
		if text, err := flowLineInputFn("Base URL: "); err != nil || text != "typed text" {
			t.Fatalf("line %q %v", text, err)
		}
		if text, err := flowSecretInputFn("API key: "); err != nil || text != "typed secret" {
			t.Fatalf("secret %q %v", text, err)
		}
		if text, err := maskedSecretPrompt("Authorization code: "); err != nil || text != "typed secret" {
			t.Fatalf("masked %q %v", text, err)
		}
		if idx, err := flowRadiolistFn("Select model", plainPickerRows([]string{"a", "b"}), PickerRadiolistOptions{CancelReturns: -1}); err != nil || idx != 1 {
			t.Fatalf("radiolist %d %v", idx, err)
		}
		if idx := setupRadiolist("Pick", []string{"x", "y"}, 0, ""); idx != 1 {
			t.Fatalf("setup radiolist %d", idx)
		}
		if picked := PromptChecklist("Tools", []string{"a", "b", "c"}, nil); !reflect.DeepEqual(picked, []int{0, 2}) {
			t.Fatalf("checklist %v", picked)
		}
		if yes := promptYesNo("Open browser? [Y/n] ", "y"); yes {
			t.Fatal("the No row must answer no")
		}
		if !runLoginCommand(context.Background(), []string{"example-cli", "login"}) || !flowStdinIsTTYFn() || !pickerConsoleFn().Interactive() {
			t.Fatal("terminal programs and menus must run through the host")
		}
		fmt.Println("printed on standard output")
		fmt.Fprintln(SetupOutput(), "Run `icebath model` again")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, p := range asked {
		kinds = append(kinds, p.Kind)
	}
	want := []string{HostedText, HostedSecret, HostedSecret, HostedChoice, HostedChoice, HostedChecklist, HostedChoice, HostedCommand}
	if !reflect.DeepEqual(kinds, want) || !reflect.DeepEqual(asked[7].Argv, []string{"example-cli", "login"}) {
		t.Fatalf("asked %v", asked)
	}
	if !strings.Contains(out.String(), "printed on standard output\n") || !strings.Contains(out.String(), "Run `/setup model` again\n") {
		t.Fatalf("output %q", out.String())
	}
	if !reflect.DeepEqual(pointers(), before) || hostedAsker() != nil {
		t.Fatal("the terminal seams were not restored")
	}
}

func TestHostedOutputKeepsTheRewrittenProgressLineApart(t *testing.T) {
	var out bytes.Buffer
	var shown []string
	trail := &hostedOutput{w: &out, progress: func(text string) { shown = append(shown, text) }}
	fmt.Fprint(trail, "\r  downloading 10%")
	fmt.Fprint(trail, "\r  downloading 60%   ")
	fmt.Fprint(trail, "\r  downloading 100%\n  ✓ Model ready\n")
	if out.String() != "  downloading 100%\n  ✓ Model ready\n" {
		t.Fatalf("lines %q", out.String())
	}
	if !reflect.DeepEqual(shown, []string{"downloading 10%", "downloading 60%", ""}) {
		t.Fatalf("progress %q", shown)
	}
}
