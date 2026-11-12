package app

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
)

// Pins the light mode cache for the test, nil forcing a fresh detection
func setLightModeCache(t *testing.T, value *bool) {
	t.Helper()
	lightModeMu.Lock()
	prev := lightModeCache
	lightModeCache = value
	lightModeMu.Unlock()
	t.Cleanup(func() {
		lightModeMu.Lock()
		lightModeCache = prev
		lightModeMu.Unlock()
	})
}

// Clears every light mode signal from the environment
func clearLightEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"ICEBATH_LIGHT", "ICEBATH_TUI_LIGHT", "ICEBATH_TUI_THEME", "ICEBATH_TUI_BACKGROUND", "COLORFGBG", "SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	setLightModeCache(t, nil)
}

func TestLightModeDetectionHermesLightEnvTrueForcesLight(t *testing.T) {
	clearLightEnv(t)
	t.Setenv("ICEBATH_LIGHT", "1")
	if !detectLightMode() {
		t.Fatal("ICEBATH_LIGHT=1 must force light")
	}
}

func TestLightModeDetectionHermesLightEnvFalseForcesDark(t *testing.T) {
	clearLightEnv(t)
	t.Setenv("ICEBATH_LIGHT", "0")
	if detectLightMode() {
		t.Fatal("ICEBATH_LIGHT=0 must force dark")
	}
}

func TestLightModeDetectionThemeHintLight(t *testing.T) {
	clearLightEnv(t)
	t.Setenv("ICEBATH_TUI_THEME", "light")
	if !detectLightMode() {
		t.Fatal("a light theme hint must read light")
	}
}

func TestLightModeDetectionBackgroundHexHintLight(t *testing.T) {
	clearLightEnv(t)
	t.Setenv("ICEBATH_TUI_BACKGROUND", "#FFFFFF")
	if !detectLightMode() {
		t.Fatal("a white background must read light")
	}
}

func TestLightModeDetectionBackgroundHexHintDark(t *testing.T) {
	clearLightEnv(t)
	t.Setenv("ICEBATH_TUI_BACKGROUND", "#1a1a2e")
	if detectLightMode() {
		t.Fatal("a dark background must read dark")
	}
}

func TestLightModeDetectionColorfgbgLightBgSlot(t *testing.T) {
	clearLightEnv(t)
	t.Setenv("COLORFGBG", "0;15")
	if !detectLightMode() {
		t.Fatal("background slot 15 must read light")
	}
}

func TestLightModeDetectionCacheIsSticky(t *testing.T) {
	clearLightEnv(t)
	t.Setenv("ICEBATH_LIGHT", "1")
	if !detectLightMode() {
		t.Fatal("first detection must be light")
	}
	t.Setenv("ICEBATH_LIGHT", "0")
	if !detectLightMode() {
		t.Fatal("the cached result must win until reset")
	}
}

// Installs a console whose streams claim to be terminals, the input a real file
func installTTYConsole(t *testing.T, in *os.File) *bytes.Buffer {
	t.Helper()
	var out, errOut bytes.Buffer
	restore := SetStreams(&cliui.Console{In: in, Out: &out, StdinTTY: func() bool { return true }, StdoutTTY: func() bool { return true }}, &errOut)
	t.Cleanup(restore)
	return &out
}

func TestOsc11ProbeSkipsOverSsh(t *testing.T) {
	for _, v := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		t.Run(v, func(t *testing.T) {
			clearLightEnv(t)
			out := installTTYConsole(t, os.Stdin)
			t.Setenv(v, "1.2.3.4 5555 22")
			if bg, ok, err := queryOSC11Background(); ok || bg != "" || err != nil {
				t.Fatalf("probed over ssh: %q", bg)
			}
			if out.Len() != 0 {
				t.Fatalf("wrote %q to the terminal over ssh", out.String())
			}
		})
	}
}

func TestOsc11ProbeSkipsWhenNotATty(t *testing.T) {
	clearLightEnv(t)
	var out, errOut bytes.Buffer
	restore := SetStreams(&cliui.Console{In: os.Stdin, Out: &out, StdinTTY: func() bool { return false }, StdoutTTY: func() bool { return true }}, &errOut)
	t.Cleanup(restore)
	if _, ok, err := queryOSC11Background(); ok || err != nil || out.Len() != 0 {
		t.Fatal("a non tty stdin must not be probed")
	}
}

func TestLightModeRemapRemapNoOpInDarkMode(t *testing.T) {
	clearLightEnv(t)
	t.Setenv("ICEBATH_LIGHT", "0")
	if got := maybeRemapForLightMode("#FFF8DC"); got != "#FFF8DC" {
		t.Fatalf("got %q", got)
	}
}

func TestLightModeRemapRemapKnownDarkColor(t *testing.T) {
	clearLightEnv(t)
	light := true
	setLightModeCache(t, &light)
	if maybeRemapForLightMode("#FFF8DC") != "#1A1A1A" || maybeRemapForLightMode("#FFD700") != "#9A6B00" {
		t.Fatal("known dark colors must remap")
	}
}

func TestLightModeRemapRemapCaseInsensitive(t *testing.T) {
	clearLightEnv(t)
	light := true
	setLightModeCache(t, &light)
	if got := maybeRemapForLightMode("#fff8dc"); got != "#1A1A1A" {
		t.Fatalf("got %q", got)
	}
}

func TestLightModeRemapRemapUnknownColorPassthrough(t *testing.T) {
	clearLightEnv(t)
	light := true
	setLightModeCache(t, &light)
	if got := maybeRemapForLightMode("#ABCDEF"); got != "#ABCDEF" {
		t.Fatalf("got %q", got)
	}
}

func TestLightModeRemapRemapSkipsStatusbarPairedColors(t *testing.T) {
	clearLightEnv(t)
	light := true
	setLightModeCache(t, &light)
	for _, fg := range []string{"#C0C0C0", "#888888", "#555555", "#8B8682"} {
		if maybeRemapForLightMode(fg) != fg {
			t.Fatalf("%s is a status bar foreground paired with a dark background and must not remap", fg)
		}
	}
}

// Installs the skin color hook for the test and removes it afterwards
func installSkinHook(t *testing.T) {
	t.Helper()
	installSkinLightModeHook()
	t.Cleanup(func() { config.InstallSkinColorFilter(nil) })
}

func TestSkinConfigHookBadgePreservesItsPairedColorsInLightMode(t *testing.T) {
	for _, skinName := range []string{"default", "sisyphus"} {
		t.Run(skinName, func(t *testing.T) {
			clearLightEnv(t)
			t.Setenv("ICEBATH_LIGHT", "1")
			installSkinHook(t)
			ctx := context.Background()
			previous := config.GetActiveSkinName(ctx)
			t.Cleanup(func() { config.SetActiveSkin(ctx, previous) })
			config.SetActiveSkin(ctx, skinName)
			skin := config.GetActiveSkin(ctx)
			background, ok := skin.Colors["status_bar_strong"]
			if !ok {
				background = "#FFD700"
				if title, ok := skin.Colors["banner_title"]; ok {
					background = title
				}
			}
			foreground, ok := skin.Colors["status_bar_bg"]
			if !ok {
				foreground = "#1a1a2e"
			}
			want := "bg:" + config.PyStr(background) + " " + config.PyStr(foreground) + " bold"
			if got := config.GetPromptToolkitStyleOverrides(ctx)["status-bar-session-title"]; got != want {
				t.Fatalf("got %q want %q", got, want)
			}
		})
	}
}

func TestSkinConfigHookHookIsIdempotent(t *testing.T) {
	clearLightEnv(t)
	light := true
	setLightModeCache(t, &light)
	installSkinHook(t)
	installSkinLightModeHook()
	skin := &config.SkinConfig{Name: "test", Colors: map[string]any{"banner_text": "#FFF8DC"}}
	if got := skin.GetColor("banner_text", ""); got != "#1A1A1A" {
		t.Fatalf("a double install must remap exactly once, got %q", got)
	}
}

func TestSkinConfigHookSkinColorRemapsThroughWrapperInLightMode(t *testing.T) {
	clearLightEnv(t)
	light := true
	setLightModeCache(t, &light)
	installSkinHook(t)
	skin := &config.SkinConfig{Name: "test", Colors: map[string]any{"banner_text": "#FFF8DC", "response_border": "#FFD700"}}
	if skin.GetColor("banner_text", "") != "#1A1A1A" || skin.GetColor("response_border", "") != "#9A6B00" {
		t.Fatal(skin.GetColor("banner_text", ""), skin.GetColor("response_border", ""))
	}
}

func TestSkinConfigHookSkinColorPassthroughInDarkMode(t *testing.T) {
	clearLightEnv(t)
	dark := false
	setLightModeCache(t, &dark)
	installSkinHook(t)
	skin := &config.SkinConfig{Name: "test", Colors: map[string]any{"banner_text": "#FFF8DC"}}
	if got := skin.GetColor("banner_text", ""); got != "#FFF8DC" {
		t.Fatalf("got %q", got)
	}
}

// Renders like a plain 80 column rich console with colors off
func renderToText(r pyrich.Renderable) string {
	var buf bytes.Buffer
	(&pyrich.Console{Out: &buf, Width: 80}).PrintRenderable(r)
	return buf.String()
}

func TestFinalAssistantContentUsesMarkdownRenderable(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	renderable := renderFinalAssistantContent("# Title\n\n- one\n- two", "render")
	if _, ok := renderable.(*pyrich.Markdown); !ok {
		t.Fatalf("got %T", renderable)
	}
	output := renderToText(renderable)
	for _, want := range []string{"Title", "one", "two"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output %q lacks %q", output, want)
		}
	}
}

func TestFinalAssistantContentKeepsNonPathMarkdownEscapes(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	output := renderToText(renderFinalAssistantContent(`1\. Not an ordered list`, "render"))
	if !strings.Contains(output, "1. Not an ordered list") || strings.Contains(output, `1\.`) {
		t.Fatalf("output %q", output)
	}
}

func TestStripModePreservesLists(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	output := renderToText(renderFinalAssistantContent("**Formatting**\n- Ran prettier\n- Files changed\n- Verified clean", "strip"))
	for _, want := range []string{"- Ran prettier", "- Files changed", "- Verified clean"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output %q lacks %q", output, want)
		}
	}
	if strings.Contains(output, "**") {
		t.Fatalf("output %q keeps bold markers", output)
	}
}

func TestStripModePreservesBlockquotes(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	output := renderToText(renderFinalAssistantContent("> This is quoted text\n> Another quoted line", "strip"))
	if !strings.Contains(output, "> This is quoted") || !strings.Contains(output, "> Another quoted") {
		t.Fatalf("output %q", output)
	}
}

func TestStripModePreservesCronAsterisksInPlainText(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	if output := renderToText(renderFinalAssistantContent("* * * * *", "strip")); !strings.Contains(output, "* * * * *") {
		t.Fatalf("output %q", output)
	}
	if output := renderToText(renderFinalAssistantContent("* * *", "strip")); strings.Contains(output, "* * *") {
		t.Fatalf("a three asterisk rule must be stripped, got %q", output)
	}
}

func TestStripModePreservesIntrawordUnderscoresInSnakeCaseIdentifiers(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	output := renderToText(renderFinalAssistantContent("Let me look at test_case_with_underscores and SOME_CONST then /tmp/snake_case_dir/file_with_name.py", "strip"))
	for _, want := range []string{"test_case_with_underscores", "SOME_CONST", "snake_case_dir", "file_with_name"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output %q lacks %q", output, want)
		}
	}
}

func TestStripModeStillStripsBoundaryUnderscoreEmphasis(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	if output := renderToText(renderFinalAssistantContent("say _hi_ and __bold__ now", "strip")); !strings.Contains(output, "say hi and bold now") {
		t.Fatalf("output %q", output)
	}
}

func TestStreamedTransformPrintsOnlyAppendedSuffix(t *testing.T) {
	got := postStreamTransformOutput("original answer\n\n[plugin appended this]", map[string]any{"response_transformed": true, "pre_transform_response": "original answer"})
	if got != "\n\n[plugin appended this]" {
		t.Fatalf("got %q", got)
	}
}

func TestStreamedTransformPrintsFullReplacementInsteadOfDroppingIt(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	got := postStreamTransformOutput("XYZ", map[string]any{"response_transformed": true, "pre_transform_response": "abc"})
	if !strings.HasSuffix(got, "\nXYZ") || strings.Contains(got, "abc") {
		t.Fatalf("got %q", got)
	}
}

func TestUntransformedStreamHasNoPostStreamOutput(t *testing.T) {
	if got := postStreamTransformOutput("original answer", map[string]any{}); got != "" {
		t.Fatalf("got %q", got)
	}
}

// Pins the output history configuration for the test and restores the defaults afterwards
func configureHistoryForTest(t *testing.T, enabled bool, maxLines int) {
	t.Helper()
	configureOutputHistory(enabled, maxLines)
	t.Cleanup(func() { configureOutputHistory(true, 200) })
}

// Texts of the recorded history entries, callables kept as their function type name
func historyTexts() []string {
	outputHistoryMu.Lock()
	defer outputHistoryMu.Unlock()
	var out []string
	for _, entry := range outputHistory {
		switch e := entry.(type) {
		case *paintedLine:
			out = append(out, e.Text)
		default:
			out = append(out, "<callable>")
		}
	}
	return out
}

func TestCprintNoAppDirectPrint(t *testing.T) {
	configureHistoryForTest(t, false, 200)
	out, _ := captureStreams(t)
	setTUIApp(nil)
	cprint("hello")
	if out.String() != "hello\n" {
		t.Fatalf("got %q", out.String())
	}
}

func TestCprintAppNotRunningDirectPrint(t *testing.T) {
	configureHistoryForTest(t, false, 200)
	out, _ := captureStreams(t)
	app := &fakeTUIApp{running: false}
	installTUIApp(t, app)
	cprint("x")
	if out.String() != "x\n" || len(app.printedAbove()) != 0 {
		t.Fatalf("got %q above %v", out.String(), app.printedAbove())
	}
}

func TestCprintSameThreadAsAppLoopDirectPrint(t *testing.T) {
	configureHistoryForTest(t, false, 200)
	out, _ := captureStreams(t)
	app := &fakeTUIApp{running: true, loop: true, onLoop: true}
	installTUIApp(t, app)
	cprint("x")
	if got := app.printedAbove(); len(got) != 1 || got[0] != "x" {
		t.Fatalf("printed above %v", got)
	}
	if len(app.loopQueue()) != 0 || out.Len() != 0 {
		t.Fatal("the app's own thread must paint directly, never through the loop queue")
	}
}

func TestCprintSwallowsPromptToolkitImportError(t *testing.T) {
	configureHistoryForTest(t, false, 200)
	out, _ := captureStreams(t)
	setTUIApp(nil)
	cprint("fallback2")
	if out.String() != "fallback2\n" {
		t.Fatalf("got %q", out.String())
	}
}

func TestReplayOutputHistoryRerendersCallableEntries(t *testing.T) {
	configureHistoryForTest(t, true, 10)
	out, _ := captureStreams(t)
	setTUIApp(nil)
	calls := 0
	render := func() []string {
		calls++
		return []string{"top border", "body"}
	}
	recordOutputHistoryEntry(render)
	replayOutputHistory(nil, nil)
	if calls != 1 || out.String() != "top border\nbody\n" {
		t.Fatalf("calls %d out %q", calls, out.String())
	}
	if got := historyTexts(); len(got) != 1 || got[0] != "<callable>" {
		t.Fatalf("history %v", got)
	}
}

func TestChatConsoleRecordsRichAnsiForResizeReplay(t *testing.T) {
	configureHistoryForTest(t, true, 10)
	captureStreams(t)
	setTUIApp(nil)
	newChatConsole().Print("[bold red]Hello[/]")
	texts := historyTexts()
	if len(texts) == 0 {
		t.Fatal("nothing recorded")
	}
	for _, line := range texts {
		if strings.Contains(line, "\x1b[") {
			return
		}
	}
	t.Fatalf("no ANSI in %q", texts)
}

func TestSuspendOutputHistoryBlocksRecording(t *testing.T) {
	configureHistoryForTest(t, true, 10)
	restore := suspendOutputHistory()
	recordOutputHistory("hidden", false)
	recordOutputHistoryEntry("also hidden")
	restore()
	if got := historyTexts(); len(got) != 0 {
		t.Fatalf("history %v", got)
	}
}

func TestClearOutputHistoryRemovesReplayableLines(t *testing.T) {
	configureHistoryForTest(t, true, 10)
	recordOutputHistory("before clear", false)
	clearOutputHistory()
	if got := historyTexts(); len(got) != 0 {
		t.Fatalf("history %v", got)
	}
}

func TestStringMessageGetsNotePrepended(t *testing.T) {
	if got := prependNoteToMessage("hello", "NOTE"); got != "NOTE\n\nhello" {
		t.Fatalf("got %q", got)
	}
}

func TestNoteIsStripped(t *testing.T) {
	if got := prependNoteToMessage("hello", "  NOTE  "); got != "NOTE\n\nhello" {
		t.Fatalf("got %q", got)
	}
}

func TestListMessageFoldsNoteIntoFirstTextPart(t *testing.T) {
	image := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:..."}}
	message := []any{map[string]any{"type": "text", "text": "describe this"}, image}
	result := prependNoteToMessage(message, "NOTE").([]any)
	first := result[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "NOTE\n\ndescribe this" {
		t.Fatalf("first %v", first)
	}
	if result[1].(map[string]any)["type"] != "image_url" || result[1].(map[string]any)["image_url"].(map[string]any)["url"] != "data:..." {
		t.Fatalf("image part %v", result[1])
	}
	if message[0].(map[string]any)["text"] != "describe this" {
		t.Fatal("the original message was mutated")
	}
}

func TestImageOnlyListGetsLeadingTextPart(t *testing.T) {
	message := []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:..."}}}
	result := prependNoteToMessage(message, "NOTE").([]any)
	if first := result[0].(map[string]any); first["type"] != "text" || first["text"] != "NOTE" {
		t.Fatalf("first %v", first)
	}
	if result[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("second %v", result[1])
	}
}

func TestUnknownShapeReturnedUnchanged(t *testing.T) {
	if prependNoteToMessage(123, "NOTE") != 123 || prependNoteToMessage(nil, "NOTE") != nil {
		t.Fatal("unknown shapes must pass through")
	}
}

func TestInterruptMarkerNotRecordedSuspendBlocksRecordingDuringCprint(t *testing.T) {
	configureHistoryForTest(t, true, 10)
	captureStreams(t)
	setTUIApp(nil)
	recordOutputHistory("before marker", false)
	restore := suspendOutputHistory()
	cprint("── [Interrupted — processing new message] ──")
	restore()
	if got := historyTexts(); len(got) != 1 || got[0] != "before marker" {
		t.Fatalf("history %v", got)
	}
}

func TestInterruptMarkerNotRecordedNormalCprintStillRecords(t *testing.T) {
	configureHistoryForTest(t, true, 10)
	out, _ := captureStreams(t)
	setTUIApp(nil)
	cprint("normal response text")
	if got := historyTexts(); len(got) != 1 || got[0] != "normal response text" || out.String() != "normal response text\n" {
		t.Fatalf("history %v out %q", got, out.String())
	}
}

func TestInterruptMarkerNotRecordedSuspendIsIdempotentNested(t *testing.T) {
	configureHistoryForTest(t, true, 10)
	recordOutputHistory("before", false)
	outer := suspendOutputHistory()
	recordOutputHistory("inside outer", false)
	inner := suspendOutputHistory()
	recordOutputHistory("inside inner", false)
	inner()
	outer()
	recordOutputHistory("after", false)
	if got := historyTexts(); strings.Join(got, "|") != "before|after" {
		t.Fatalf("history %v", got)
	}
}

func TestReplayDoesNotDuplicateMarkerReplayCleanAfterInterruptedTurn(t *testing.T) {
	configureHistoryForTest(t, true, 10)
	out, _ := captureStreams(t)
	setTUIApp(nil)
	recordOutputHistory("Assistant response text", false)
	restore := suspendOutputHistory()
	cprint("── [Interrupted — processing new message] ──")
	restore()
	if got := historyTexts(); len(got) != 1 || got[0] != "Assistant response text" {
		t.Fatalf("history %v", got)
	}
	replayOutputHistory(nil, nil)
	if n := strings.Count(out.String(), "Interrupted"); n != 1 {
		t.Fatalf("marker printed %d times: %q", n, out.String())
	}
	if !strings.Contains(out.String(), "Assistant response text") {
		t.Fatalf("replay missing the response: %q", out.String())
	}
}

func TestCompactBannerSkinIntegrationPoseidonCompactBannerUsesSkinColors(t *testing.T) {
	clearLightEnv(t)
	t.Setenv("COLUMNS", "90")
	ctx := context.Background()
	previous := config.GetActiveSkinName(ctx)
	t.Cleanup(func() { config.SetActiveSkin(ctx, previous) })
	config.SetActiveSkin(ctx, "poseidon")
	skin := config.GetActiveSkin(ctx)
	prev := bannerVersionLabel
	bannerVersionLabel = func() string { return "Icebath Agent v0.1.0 (test)" }
	t.Cleanup(func() { bannerVersionLabel = prev })
	banner := buildCompactBanner()
	for _, want := range []string{skin.GetBranding("agent_name", ""), skin.GetColor("banner_border", ""), skin.GetColor("banner_title", ""), skin.GetColor("banner_dim", "")} {
		if want == "" || !strings.Contains(banner, want) {
			t.Fatalf("banner %q lacks %q", banner, want)
		}
	}
}

func TestAnsiRichTextHelperPreservesLiteralBrackets(t *testing.T) {
	if got := richPlainFromANSI("[notatag] literal"); got != "[notatag] literal" {
		t.Fatalf("got %q", got)
	}
}

func TestAnsiRichTextHelperStripsAnsiButKeepsPlainText(t *testing.T) {
	if got := richPlainFromANSI("\x1b[31mred\x1b[0m"); got != "red" {
		t.Fatalf("got %q", got)
	}
}
