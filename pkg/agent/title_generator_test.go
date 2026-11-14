package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Replaces a title seam for one test, joining background upgrades before restoring it
func swapTitleSeam[T any](t *testing.T, target *T, v T) {
	t.Helper()
	prev := *target
	*target = v
	t.Cleanup(func() {
		waitForTitleUpgrades(10 * time.Second)
		*target = prev
	})
}

// Isolated home with an empty config the title module reads
func titleTestHome(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	swapTitleSeam(t, &titleLoadConfig, func(context.Context) (map[string]any, error) { return map[string]any{}, nil })
}

// Wire shaped auxiliary response whose first choice carries message
func titleResponse(message map[string]any) map[string]any {
	return map[string]any{"choices": []any{map[string]any{"message": message}}}
}

// Title model double returning content and counting its calls
func fakeTitleLLMContent(t *testing.T, content any) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	swapTitleSeam(t, &titleCallLLM, func(context.Context, map[string]any) (any, error) {
		calls.Add(1)
		return titleResponse(map[string]any{"content": content}), nil
	})
	return &calls
}

// Background title stage double counting its runs
func fakeAutoTitleSession(t *testing.T) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	swapTitleSeam(t, &autoTitleSessionFn, func(context.Context, any, string, string, autoTitleSessionOptions) { calls.Add(1) })
	return &calls
}

// Session store under a temp dir holding the given sessions
func newTitleTestDB(t *testing.T, source string, ids ...string) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, id := range ids {
		if err := db.CreateSession(state.NewSession{ID: id, Source: source}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// Title of a session, nil standing for an untitled one
func titleOf(t *testing.T, db *state.DB, id string) *string {
	t.Helper()
	title, ok, err := db.GetSessionTitle(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return nil
	}
	return &title
}

// Provenance of a session title, empty for an untitled one
func titleSourceOf(t *testing.T, db *state.DB, id string) string {
	t.Helper()
	source, _, err := db.GetSessionTitleSource(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func sp(s string) *string { return &s }

type fakeSetAutoResult struct {
	ok  bool
	err error
}

// Duck typed session store recording its title writes
type fakeTitleDB struct {
	mu           sync.Mutex
	sourceSet    bool
	source       string
	setAuto      []fakeSetAutoResult
	setAutoCalls [][3]string
	next         string
	nextCalls    []string
}

func (f *fakeTitleDB) GetSessionTitleSource(context.Context, string) (string, bool, error) {
	return f.source, f.sourceSet, nil
}

func (f *fakeTitleDB) GetSessionTitle(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (f *fakeTitleDB) SetAutoTitleContext(_ context.Context, sessionID, title, source string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setAutoCalls = append(f.setAutoCalls, [3]string{sessionID, title, source})
	if len(f.setAuto) > 0 {
		r := f.setAuto[0]
		f.setAuto = f.setAuto[1:]
		return r.ok, r.err
	}
	return true, nil
}

func (f *fakeTitleDB) GetNextTitleInLineage(_ context.Context, base string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextCalls = append(f.nextCalls, base)
	return f.next, nil
}

func TestGenerateTitleGeneratedPastePreviewReachesTheSharedTitleInput(t *testing.T) {
	cases := []struct {
		instruction, preview string
		parts                []string
	}{
		{"@file:/tmp/composer-pastes/pasted_content_1.txt", "Quarterly incident analysis for the database migration", []string{"Quarterly incident analysis"}},
		{"Analyze this", "Quarterly incident analysis for the database migration", []string{"Analyze this", "Quarterly incident analysis"}},
		{"Prepare the deployment follow-up", "Quarterly incident analysis", []string{"Prepare the deployment follow-up", "Quarterly incident analysis"}},
	}
	for _, c := range cases {
		input := buildTitleInput(c.instruction, sp(c.preview))
		for _, part := range c.parts {
			if !strings.Contains(input, part) {
				t.Fatalf("%q missing from %q", part, input)
			}
		}
		if strings.Contains(input, "@file:") {
			t.Fatalf("ref leaked into %q", input)
		}
		lead := c.instruction
		if strings.HasPrefix(c.instruction, "@file:") {
			lead = c.preview
		}
		if got := deriveTitle(c.instruction, sp(c.preview)); !strings.HasPrefix(got, lead[:12]) {
			t.Fatalf("derived %q does not lead with %q", got, lead[:12])
		}
	}
}

func TestGenerateTitleExpandedPasteRefFooterDoesNotDemoteThePreview(t *testing.T) {
	ref := "@file:/home/u/.icebath/attachments/pasted_content_2026-09-18_14-09-43-735_d0ee85.txt"
	preview := "Quarterly incident analysis for the database cluster"
	for _, footer := range []string{"\n\n--- Context Warnings ---\n- " + ref + ": path is outside the allowed workspace",
		"\n\n--- Attached Context ---\n\n### file: pasted_content.txt\n" + preview} {
		input := buildTitleInput(ref+footer, sp(preview))
		if !strings.HasPrefix(input, preview) || strings.Contains(input, "---") || strings.Contains(input, "@file:") {
			t.Fatalf("input %q", input)
		}
		if got := deriveTitle(ref+footer, sp(preview)); !strings.HasPrefix(got, "Quarterly incident analysis") {
			t.Fatalf("derived %q", got)
		}
	}
}

func TestGenerateTitleManualAttachmentOnlyOpenerYieldsNoPathTitle(t *testing.T) {
	titleTestHome(t)
	calls := fakeTitleLLMContent(t, `{"title": "Report pdf"}`)
	msg := "@file:AppData/Local/icebath/profiles/local/attachments/report.pdf"
	if isTitleableUserMessage(msg) {
		t.Fatal("attachment only opener is titleable")
	}
	if deriveTitle(msg, nil) != "" || deriveTitle(msg+"\n\n--- Attached Context ---\n(file content)", nil) != "" {
		t.Fatal("derived a path title")
	}
	for _, m := range []string{"@file:`my file.txt`", "@folder:/some/dir", "@file:`my file.txt`:3", `@file:"spaced name.md":12-14`, "@file:'single quoted.md':1"} {
		if isTitleableUserMessage(m) {
			t.Fatalf("%q is titleable", m)
		}
	}
	if got := generateTitle(context.Background(), msg, generateTitleOptions{}); got != "" || calls.Load() != 0 {
		t.Fatalf("model titled a file drop %q after %d calls", got, calls.Load())
	}
}

func TestGenerateTitleAttachmentPlusInstructionTitlesFromTheInstruction(t *testing.T) {
	msg := "Review @file:notes.txt and fix the off-by-one"
	if !isTitleableUserMessage(msg) {
		t.Fatal("instruction with attachment is not titleable")
	}
	if got := deriveTitle(msg, nil); got != msg {
		t.Fatalf("derived %q", got)
	}
}

func TestGenerateTitleTitleInputBudgetAndManualAttachmentsStayUnread(t *testing.T) {
	input := buildTitleInput("Describe the release plan", sp(strings.Repeat("p", maxTitleInputChars)))
	if pystr.Len(input) != maxTitleInputChars || !strings.HasPrefix(input, "Describe the release plan") || !strings.HasSuffix(input, strings.Repeat("p", 20)) {
		t.Fatalf("input %q", input)
	}
	if got := buildTitleInput("Summarize @file:notes.txt", nil); got != "Summarize @file:notes.txt" {
		t.Fatalf("input %q", got)
	}
}

func TestGenerateTitleTitleLanguageReadsConfig(t *testing.T) {
	ctx := context.Background()
	cfg := map[string]any{"auxiliary": map[string]any{"title_generation": map[string]any{"language": "  French "}}}
	swapTitleSeam(t, &titleLoadConfig, func(context.Context) (map[string]any, error) { return cfg, nil })
	if got := titleLanguage(ctx); got != "French" {
		t.Fatalf("language %q", got)
	}
	titleLoadConfig = func(context.Context) (map[string]any, error) { return map[string]any{}, nil }
	if got := titleLanguage(ctx); got != "" {
		t.Fatalf("language %q", got)
	}
	titleLoadConfig = func(context.Context) (map[string]any, error) { return nil, errors.New("bad config") }
	if got := titleLanguage(ctx); got != "" {
		t.Fatalf("language %q", got)
	}
}

func TestGenerateTitleGenerateTitleDisablesReasoning(t *testing.T) {
	titleTestHome(t)
	var captured map[string]any
	swapTitleSeam(t, &titleCallLLM, func(_ context.Context, kwargs map[string]any) (any, error) {
		captured = kwargs
		return titleResponse(map[string]any{"content": `{"title": "Reasoning Off"}`}), nil
	})
	if got := generateTitle(context.Background(), "question", generateTitleOptions{}); got != "Reasoning Off" {
		t.Fatalf("title %q", got)
	}
	if !reflect.DeepEqual(captured["reasoning_config"], map[string]any{"enabled": false}) {
		t.Fatalf("reasoning_config %#v", captured["reasoning_config"])
	}
}

func TestGenerateTitleTruncatedStructuredOutputNeverBecomesTheTitle(t *testing.T) {
	titleTestHome(t)
	cases := []struct{ content, want string }{
		{`{"title":"Investigate and fix the login butt`, ""},
		{"```json", ""},
		{`{"title"`, ""},
		{"*Fix the login flow*", "*Fix the login flow*"},
		{"修复登录按钮", "修复登录按钮"},
		{"42", "42"},
		{"```json\n{\"title\": \"Fix login button\"", "Fix login button"},
		{"[WIP] Fix login flow", "[WIP] Fix login flow"},
		{"Fix ``` rendering in chat", "Fix ``` rendering in chat"},
	}
	for _, c := range cases {
		content := c.content
		swapTitleSeam(t, &titleCallLLM, func(context.Context, map[string]any) (any, error) {
			return titleResponse(map[string]any{"content": content}), nil
		})
		if got := generateTitle(context.Background(), "login is broken", generateTitleOptions{}); got != c.want {
			t.Fatalf("content %q titled %q, want %q", c.content, got, c.want)
		}
	}
}

func TestGenerateTitleJSONInReasoningContentIsUsedButReasoningProseIsNot(t *testing.T) {
	titleTestHome(t)
	cases := []struct {
		message map[string]any
		want    string
	}{
		{map[string]any{"content": "", "reasoning_content": `{"title": "Check FFmpeg on this machine"}`}, "Check FFmpeg on this machine"},
		{map[string]any{"content": nil, "reasoning": `{"title": "Check FFmpeg on this machine"}`}, "Check FFmpeg on this machine"},
		{map[string]any{"content": "", "reasoning_content": "The user wants ffmpeg checked. A short title would be"}, ""},
	}
	for _, c := range cases {
		message := c.message
		swapTitleSeam(t, &titleCallLLM, func(context.Context, map[string]any) (any, error) { return titleResponse(message), nil })
		if got := generateTitle(context.Background(), "check ffmpeg", generateTitleOptions{}); got != c.want {
			t.Fatalf("message %v titled %q", c.message, got)
		}
	}
}

func TestGenerateTitleStripsThinkBlocks(t *testing.T) {
	titleTestHome(t)
	fakeTitleLLMContent(t, "<think>The user wants a title. I'll summarize the topic concisely.</think>Debugging Python Import Errors")
	got := generateTitle(context.Background(), "help me fix this import", generateTitleOptions{})
	if got != "Debugging Python Import Errors" || strings.Contains(got, "<think>") || strings.Contains(got, "summarize") {
		t.Fatalf("title %q", got)
	}
}

func TestGenerateTitleStripsUnterminatedThinkBlock(t *testing.T) {
	titleTestHome(t)
	fakeTitleLLMContent(t, "<think>Let me reason about a good title for this session")
	if got := generateTitle(context.Background(), "hello", generateTitleOptions{}); got != "" {
		t.Fatalf("title %q", got)
	}
}

func TestGenerateTitleTruncatesLongTitles(t *testing.T) {
	titleTestHome(t)
	fakeTitleLLMContent(t, strings.Repeat("A", 100))
	got := generateTitle(context.Background(), "question", generateTitleOptions{})
	if pystr.Len(got) != 80 || !strings.HasSuffix(got, "...") {
		t.Fatalf("title %q", got)
	}
}

func TestGenerateTitleRejectsAnswerShapedOutput(t *testing.T) {
	titleTestHome(t)
	fakeTitleLLMContent(t, "I don't have context on a \"registration system\" - that's not "+
		"something I recognize from this conversation, and I don't see "+
		"any prior discussion or code about it here")
	if got := generateTitle(context.Background(), "how does the registration system work?", generateTitleOptions{}); got != "" {
		t.Fatalf("title %q", got)
	}
}

func TestGenerateTitleRejectsManyShortWords(t *testing.T) {
	titleTestHome(t)
	fakeTitleLLMContent(t, "one two three four five six seven eight nine ten eleven twelve thirteen")
	if got := generateTitle(context.Background(), "question", generateTitleOptions{}); got != "" {
		t.Fatalf("title %q", got)
	}
}

func TestGenerateTitleAcceptsNormalTitle(t *testing.T) {
	titleTestHome(t)
	fakeTitleLLMContent(t, "Investigate the title resolver bug")
	if got := generateTitle(context.Background(), "question", generateTitleOptions{}); got != "Investigate the title resolver bug" {
		t.Fatalf("title %q", got)
	}
}

func TestGenerateTitleRejectsPromptExampleEcho(t *testing.T) {
	titleTestHome(t)
	examples := make([]string, 0, len(exampleEchoReject))
	for e := range exampleEchoReject {
		examples = append(examples, e)
	}
	sort.Strings(examples)
	for _, example := range examples {
		for _, wrap := range []string{"%s", `"%s"`, "(%s)", "[%s]"} {
			echo := fmt.Sprintf(wrap, example)
			swapTitleSeam(t, &titleCallLLM, func(context.Context, map[string]any) (any, error) {
				return titleResponse(map[string]any{"content": echo}), nil
			})
			if got := generateTitle(context.Background(), "help me with something unrelated", generateTitleOptions{}); got != "" {
				t.Fatalf("echo %q titled %q", echo, got)
			}
		}
	}
}

func TestGenerateTitleFriendlyGreetingExampleIsAllowed(t *testing.T) {
	titleTestHome(t)
	fakeTitleLLMContent(t, "Friendly greeting")
	if got := generateTitle(context.Background(), "hey there!", generateTitleOptions{}); got != "Friendly greeting" {
		t.Fatalf("title %q", got)
	}
}

func TestGenerateTitleTopicalTitleResemblingExamplePasses(t *testing.T) {
	titleTestHome(t)
	fakeTitleLLMContent(t, "Fix login button on desktop")
	if got := generateTitle(context.Background(), "the login button is broken on desktop", generateTitleOptions{}); got != "Fix login button on desktop" {
		t.Fatalf("title %q", got)
	}
}

func TestGenerateTitleInvokesFailureCallbackOnException(t *testing.T) {
	titleTestHome(t)
	exc := errors.New("openrouter 402: credits exhausted")
	swapTitleSeam(t, &titleCallLLM, func(context.Context, map[string]any) (any, error) { return nil, exc })
	type failure struct {
		task string
		err  error
	}
	var captured []failure
	cb := func(task string, err error) { captured = append(captured, failure{task, err}) }
	if got := generateTitle(context.Background(), "question", generateTitleOptions{FailureCallback: cb}); got != "" {
		t.Fatalf("title %q", got)
	}
	if len(captured) != 1 || captured[0].task != "title generation" || captured[0].err != exc {
		t.Fatalf("captured %v", captured)
	}
}

func TestAutoTitleSessionDoesNotOverwriteTitleSetImmediatelyBeforeConditionalWrite(t *testing.T) {
	titleTestHome(t)
	db := newTitleTestDB(t, "cli", "sess-1")
	swapTitleSeam(t, &generateTitleFn, func(ctx context.Context, _ string, _ generateTitleOptions) string {
		if _, err := db.SetSessionTitle(ctx, "sess-1", "Manual Title"); err != nil {
			t.Error(err)
		}
		return "Auto Title"
	})
	var seen []string
	autoTitleSession(context.Background(), db, "sess-1", "hi", autoTitleSessionOptions{TitleCallback: func(title, _ string) { seen = append(seen, title) }})
	if got := titleOf(t, db, "sess-1"); got == nil || *got != "Manual Title" || len(seen) != 0 {
		t.Fatalf("title %v seen %v", got, seen)
	}
}

func TestAutoTitleSessionInvokesTitleCallbackAfterSettingTitle(t *testing.T) {
	titleTestHome(t)
	db := &fakeTitleDB{}
	swapTitleSeam(t, &generateTitleFn, func(context.Context, string, generateTitleOptions) string { return "Readable Session" })
	var seen [][2]string
	autoTitleSession(context.Background(), db, "sess-1", "hello", autoTitleSessionOptions{TitleCallback: func(title, source string) {
		seen = append(seen, [2]string{title, source})
	}})
	if !reflect.DeepEqual(db.setAutoCalls, [][3]string{{"sess-1", "Readable Session", "llm"}}) {
		t.Fatalf("set_auto_title calls %v", db.setAutoCalls)
	}
	if !reflect.DeepEqual(seen, [][2]string{{"Readable Session", "llm"}}) {
		t.Fatalf("seen %v", seen)
	}
}

func TestAutoTitleSessionUpgradesADerivedTitleButNotAnLlmOne(t *testing.T) {
	titleTestHome(t)
	ctx := context.Background()
	db := newTitleTestDB(t, "cli", "sess-1")
	if _, err := db.SetAutoTitleContext(ctx, "sess-1", "fix the flaky auth test", "derived"); err != nil {
		t.Fatal(err)
	}
	swapTitleSeam(t, &generateTitleFn, func(context.Context, string, generateTitleOptions) string { return "Fix flaky auth test" })
	autoTitleSession(ctx, db, "sess-1", "fix the flaky auth test", autoTitleSessionOptions{})
	if got := titleOf(t, db, "sess-1"); got == nil || *got != "Fix flaky auth test" {
		t.Fatalf("title %v", got)
	}
	generateTitleFn = func(context.Context, string, generateTitleOptions) string { return "Totally Different" }
	autoTitleSession(ctx, db, "sess-1", "fix the flaky auth test", autoTitleSessionOptions{})
	if got := titleOf(t, db, "sess-1"); got == nil || *got != "Fix flaky auth test" {
		t.Fatalf("title %v", got)
	}
}

func TestAutoTitleSessionBodyExceptionRoutedToFailureCallback(t *testing.T) {
	titleTestHome(t)
	boom := &pyint.Error{Kind: "ImportError", Msg: "stale module"}
	swapTitleSeam(t, &generateTitleFn, func(context.Context, string, generateTitleOptions) string { panic(boom) })
	type failure struct {
		task string
		err  error
	}
	var seen []failure
	autoTitleSession(context.Background(), &fakeTitleDB{}, "sess-1", "hi", autoTitleSessionOptions{FailureCallback: func(task string, err error) {
		seen = append(seen, failure{task, err})
	}})
	if len(seen) != 1 || seen[0].task != "title generation" || seen[0].err != error(boom) {
		t.Fatalf("seen %v", seen)
	}
}

// Waits for a signal channel up to timeout
func titleSignalled(ch <-chan struct{}, timeout time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}

func TestMaybeAutoTitleTitleCallWaitsForTheTurnWhenItSharesASelfHostedEndpoint(t *testing.T) {
	titleTestHome(t)
	cases := []struct {
		runtime  map[string]any
		cfg      map[string]any
		deferred bool
	}{
		{map[string]any{"provider": "custom", "base_url": "http://127.0.0.1:8080/v1"}, map[string]any{}, true},
		{map[string]any{"provider": "custom:gptoss-local", "base_url": "http://127.0.0.1:8080/v1"}, map[string]any{}, true},
		{map[string]any{"provider": "lmstudio", "base_url": "http://127.0.0.1:1234/v1"}, map[string]any{}, true},
		{map[string]any{"provider": "custom", "base_url": "http://127.0.0.1:8080/v1"}, map[string]any{"base_url": "http://127.0.0.1:8080/v1/"}, true},
		{map[string]any{"provider": "custom", "base_url": "http://127.0.0.1:8080/v1"}, map[string]any{"provider": "openrouter"}, false},
		{map[string]any{"provider": "custom", "base_url": "http://127.0.0.1:8080/v1"}, map[string]any{"base_url": "http://10.0.0.2:8080/v1"}, false},
		{map[string]any{"provider": "openrouter", "base_url": "https://openrouter.ai/api/v1"}, map[string]any{}, false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.runtime["provider"], c.cfg), func(t *testing.T) {
			cfg := c.cfg
			swapTitleSeam(t, &titleConfigFn, func(context.Context) (map[string]any, error) { return cfg, nil })
			started := make(chan struct{})
			var once sync.Once
			swapTitleSeam(t, &autoTitleSessionFn, func(context.Context, any, string, string, autoTitleSessionOptions) {
				once.Do(func() { close(started) })
			})
			upgrade, startedNow := maybeAutoTitle(context.Background(), &fakeTitleDB{}, "sess-1", "hello", autoTitleOptions{
				ConversationHistory: []*state.Message{{Role: "user", Content: "hello"}}, MainRuntime: c.runtime})
			thread, ok := upgrade.(*memory.ContextThread)
			if !ok || thread == nil {
				t.Fatalf("upgrade %#v", upgrade)
			}
			if c.deferred {
				if startedNow || titleUpgradeStarted(thread) || titleSignalled(started, 300*time.Millisecond) {
					t.Fatal("title request went out during the turn")
				}
				startTitleUpgrade(thread)
			}
			if !titleSignalled(started, 10*time.Second) {
				t.Fatal("auto_title thread never ran")
			}
			if !titleUpgradeStarted(thread) {
				t.Fatal("started upgrade is not joinable")
			}
		})
	}
}

func TestMaybeAutoTitleTitlePinNamingTheSameSelfHostedRouteStillWaitsForTheTurn(t *testing.T) {
	keyed := map[string]any{"providers": map[string]any{"gptoss": map[string]any{"name": "GPTOSS Local", "base_url": "http://127.0.0.1:8080/v1"}}}
	swapTitleSeam(t, &titleLoadConfig, func(context.Context) (map[string]any, error) { return keyed, nil })
	cases := []struct {
		main     string
		cfg      map[string]any
		deferred bool
	}{
		{"Custom:GPTOSS-Local", map[string]any{}, true},
		{"custom:gptoss-local", map[string]any{"provider": "custom:gptoss-local"}, true},
		{"custom:gptoss-local", map[string]any{"provider": "gptoss-local"}, true},
		{"custom:gptoss-local", map[string]any{"provider": "GPTOSS Local"}, true},
		{"custom:gptoss", map[string]any{"provider": "GPTOSS Local"}, true},
		{"custom:gptoss-local", map[string]any{"provider": "custom"}, true},
		{"custom:gptoss-local", map[string]any{"provider": "custom:gptoss-local", "base_url": "http://127.0.0.1:8080/v1/"}, true},
		{"custom:gptoss-local", map[string]any{"provider": "custom:other", "base_url": "http://10.0.0.2:8080/v1"}, false},
		{"custom:gptoss-local", map[string]any{"provider": "gptoss-local", "base_url": "http://10.0.0.2:8080/v1"}, false},
		{"custom:gptoss-local", map[string]any{"provider": "openrouter"}, false},
		{"custom:gptoss-local", map[string]any{"provider": "nous"}, false},
		{"custom", map[string]any{"provider": "ollama"}, true},
		{"custom", map[string]any{"provider": "vllm"}, true},
		{"lmstudio", map[string]any{"provider": "lm-studio"}, true},
		{"lmstudio", map[string]any{"provider": "lmstudio", "base_url": "http://10.0.0.2:1234/v1"}, false},
		{"lmstudio", map[string]any{"provider": "openrouter"}, false},
	}
	for _, c := range cases {
		cfg := c.cfg
		swapTitleSeam(t, &titleConfigFn, func(context.Context) (map[string]any, error) { return cfg, nil })
		rt := map[string]any{"provider": c.main, "base_url": "http://127.0.0.1:8080/v1"}
		if got := titleUpgradeMustWaitForTurn(context.Background(), rt); got != c.deferred {
			t.Fatalf("main %q pin %v waits %v, want %v", c.main, c.cfg, got, c.deferred)
		}
	}
}

// Isolated kanban home with the default board
func titleKanbanHome(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_KANBAN_HOME", h)
	t.Cleanup(home.PatchUserHome(tmp))
	swapTitleSeam(t, &titleLoadConfig, func(context.Context) (map[string]any, error) { return map[string]any{}, nil })
}

// Creates a card on the default board and returns its id
func createTitleKanbanCard(t *testing.T, title string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	err := kanban.ConnectClosing(ctx, "", kanban.BoardArg("default"), func(conn state.SQLiteConn) error {
		var err error
		id, err = kanban.CreateTask(ctx, conn, kanban.CreateTaskOptions{Title: title, Board: kanban.BoardArg("default")})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMaybeAutoTitleKanbanWorkerIsNamedAfterItsCardWithoutTheLlmThread(t *testing.T) {
	titleKanbanHome(t)
	taskID := createTitleKanbanCard(t, "Fix flaky worker startup")
	t.Setenv("ICEBATH_KANBAN_TASK", taskID)
	db := newTitleTestDB(t, "kanban", "sess-1")
	calls := fakeAutoTitleSession(t)
	maybeAutoTitle(context.Background(), db, "sess-1", "work kanban task "+taskID, autoTitleOptions{})
	waitForTitleUpgrades(10 * time.Second)
	if got := titleOf(t, db, "sess-1"); got == nil || *got != "Fix flaky worker startup" {
		t.Fatalf("title %v", got)
	}
	if src := titleSourceOf(t, db, "sess-1"); src != "llm" || calls.Load() != 0 {
		t.Fatalf("source %q calls %d", src, calls.Load())
	}
}

func TestMaybeAutoTitleKanbanWorkerWithAnOverlongCardTitleIsStillNamed(t *testing.T) {
	titleKanbanHome(t)
	card := "Investigate why the swap modal intermittently fails to render its confirmation step on mobile Safari after a retry"
	if pystr.Len(card) <= state.MaxTitleLength {
		t.Fatal("card is not overlong")
	}
	taskID := createTitleKanbanCard(t, card)
	t.Setenv("ICEBATH_KANBAN_TASK", taskID)
	db := newTitleTestDB(t, "kanban")
	fakeAutoTitleSession(t)
	for _, sid := range []string{"sess-1", "sess-2"} {
		if err := db.CreateSession(state.NewSession{ID: sid, Source: "kanban"}); err != nil {
			t.Fatal(err)
		}
		maybeAutoTitle(context.Background(), db, sid, "work kanban task "+taskID, autoTitleOptions{})
	}
	first, second := titleOf(t, db, "sess-1"), titleOf(t, db, "sess-2")
	if first == nil || !strings.HasPrefix(*first, card[:40]) || !strings.HasSuffix(*first, "…") {
		t.Fatalf("first %v", first)
	}
	if second == nil || *second != *first+" #2" || pystr.Len(*second) > state.MaxTitleLength {
		t.Fatalf("second %v", second)
	}
}

func TestMaybeAutoTitleKanbanWorkerWithUnreadableCardFallsBackToTheTaskID(t *testing.T) {
	titleKanbanHome(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_missing")
	db := newTitleTestDB(t, "kanban", "sess-1")
	calls := fakeAutoTitleSession(t)
	maybeAutoTitle(context.Background(), db, "sess-1", "work kanban task t_missing", autoTitleOptions{})
	waitForTitleUpgrades(10 * time.Second)
	if got := titleOf(t, db, "sess-1"); got == nil || *got != "Kanban task t_missing" || calls.Load() != 0 {
		t.Fatalf("title %v calls %d", got, calls.Load())
	}
}

func TestMaybeAutoTitleDelegatedChildOfAWorkerIsNotNamedAfterTheCard(t *testing.T) {
	titleKanbanHome(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_parent")
	db := newTitleTestDB(t, "kanban", "child-1")
	calls := fakeAutoTitleSession(t)
	env := &tools.Env{Values: &sync.Map{}}
	env.Values.Store("session.delegated_child", true)
	maybeAutoTitle(context.Background(), db, "child-1", "research the auth flow for the parent", autoTitleOptions{Env: env})
	waitForTitleUpgrades(10 * time.Second)
	if got := titleOf(t, db, "child-1"); got != nil && *got == "Kanban task t_parent" {
		t.Fatal("delegated child named after the card")
	}
	if calls.Load() != 1 {
		t.Fatalf("auto title ran %d times", calls.Load())
	}
}

func TestMaybeAutoTitleWritesInstantTitleBeforeTheModelRuns(t *testing.T) {
	titleTestHome(t)
	db := newTitleTestDB(t, "cli", "sess-1")
	fakeAutoTitleSession(t)
	maybeAutoTitle(context.Background(), db, "sess-1", "fix the flaky auth test in login", autoTitleOptions{})
	if got := titleOf(t, db, "sess-1"); got == nil || *got != "fix the flaky auth test in login" || titleSourceOf(t, db, "sess-1") != "derived" {
		t.Fatalf("title %v", got)
	}
}

func TestMaybeAutoTitleModelUpgradeDisabledKeepsDerivedTitleAndSpawnsNoThread(t *testing.T) {
	titleTestHome(t)
	db := newTitleTestDB(t, "cli", "sess-1")
	cfg := map[string]any{"auxiliary": map[string]any{"title_generation": map[string]any{"enabled": true, "model_upgrade_enabled": false}}}
	swapTitleSeam(t, &titleLoadConfig, func(context.Context) (map[string]any, error) { return cfg, nil })
	auto := fakeAutoTitleSession(t)
	llm := fakeTitleLLMContent(t, `{"title": "Repair startup memory routing"}`)
	upgrade, _ := maybeAutoTitle(context.Background(), db, "sess-1", "repair startup memory routing", autoTitleOptions{})
	if got := titleOf(t, db, "sess-1"); got == nil || *got != "repair startup memory routing" || titleSourceOf(t, db, "sess-1") != "derived" {
		t.Fatalf("title %v", got)
	}
	if upgrade != nil || auto.Load() != 0 || llm.Load() != 0 {
		t.Fatalf("upgrade %v auto %d llm %d", upgrade, auto.Load(), llm.Load())
	}
	if got := generateTitle(context.Background(), "repair startup memory routing", generateTitleOptions{}); got != "Repair startup memory routing" {
		t.Fatalf("explicit title %q", got)
	}
}

func TestMaybeAutoTitleEnabledFalseStillDisablesDerivedAndModelTitles(t *testing.T) {
	titleTestHome(t)
	db := newTitleTestDB(t, "cli", "sess-1")
	cfg := map[string]any{"auxiliary": map[string]any{"title_generation": map[string]any{"enabled": false, "model_upgrade_enabled": true}}}
	swapTitleSeam(t, &titleLoadConfig, func(context.Context) (map[string]any, error) { return cfg, nil })
	auto := fakeAutoTitleSession(t)
	llm := fakeTitleLLMContent(t, `{"title": "x"}`)
	upgrade, _ := maybeAutoTitle(context.Background(), db, "sess-1", "repair startup memory routing", autoTitleOptions{})
	if got := titleOf(t, db, "sess-1"); got != nil || upgrade != nil || auto.Load() != 0 || llm.Load() != 0 {
		t.Fatalf("title %v upgrade %v auto %d llm %d", got, upgrade, auto.Load(), llm.Load())
	}
}

func TestMaybeAutoTitleSkipsEveryShapeOfMachineAuthoredOpener(t *testing.T) {
	titleTestHome(t)
	for _, opener := range []string{
		"[CONTEXT COMPACTION — REFERENCE ONLY] Earlier turns were compacted",
		"[CONTEXT SUMMARY]: the user was refactoring the auth module",
		"[System note: the user switched models]",
		"[Runtime note: resumed from checkpoint]",
	} {
		db := newTitleTestDB(t, "cli", "sess-1")
		calls := fakeAutoTitleSession(t)
		maybeAutoTitle(context.Background(), db, "sess-1", opener, autoTitleOptions{})
		waitForTitleUpgrades(10 * time.Second)
		if got := titleOf(t, db, "sess-1"); got != nil || calls.Load() != 0 {
			t.Fatalf("opener %q titled %v", opener, got)
		}
	}
}

func TestMaybeAutoTitleAMultimodalTurnCountsAsARealQuestion(t *testing.T) {
	if !titleIsRealUserTurn(&state.Message{Role: "user", Content: []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,x"}},
		map[string]any{"type": "text", "text": "fix the login button"},
	}}) {
		t.Fatal("multimodal question not counted")
	}
	if titleIsRealUserTurn(&state.Message{Role: "user", Content: []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x"}}}}) {
		t.Fatal("wordless image counted as a question")
	}
}

func TestMaybeAutoTitleTitlesOnALaterTurnWhenTheOpenerWasNotTitleable(t *testing.T) {
	titleTestHome(t)
	db := newTitleTestDB(t, "cli", "sess-1")
	fakeAutoTitleSession(t)
	history := []*state.Message{
		{Role: "user", Content: "[CONTEXT COMPACTION — REFERENCE ONLY] x"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "thanks"},
		{Role: "assistant", Content: "sure"},
	}
	maybeAutoTitle(context.Background(), db, "sess-1", "fix the flaky auth test", autoTitleOptions{ConversationHistory: history})
	if got := titleOf(t, db, "sess-1"); got == nil || *got != "fix the flaky auth test" {
		t.Fatalf("title %v", got)
	}
}

func TestMaybeAutoTitleLeavesAnAlreadyTitledSessionAloneOnLaterTurns(t *testing.T) {
	titleTestHome(t)
	ctx := context.Background()
	db := newTitleTestDB(t, "cli", "sess-1")
	if _, err := db.SetSessionTitle(ctx, "sess-1", "Existing name"); err != nil {
		t.Fatal(err)
	}
	calls := fakeAutoTitleSession(t)
	history := []*state.Message{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi"}, {Role: "user", Content: "thanks"}, {Role: "assistant", Content: "sure"}}
	maybeAutoTitle(ctx, db, "sess-1", "and now something else", autoTitleOptions{ConversationHistory: history})
	waitForTitleUpgrades(10 * time.Second)
	if got := titleOf(t, db, "sess-1"); got == nil || *got != "Existing name" || calls.Load() != 0 {
		t.Fatalf("title %v calls %d", got, calls.Load())
	}
}

func TestMaybeAutoTitleProvisionalGreeting(t *testing.T) {
	t.Run("test_only_the_exact_greeting_placeholder_is_provisional", func(t *testing.T) {
		for title, want := range map[string]bool{
			"Friendly greeting": true, "'Friendly greeting in chat'": true,
			"Friendly greeting card design": false, "Friendly greetings and pleasantries": false,
		} {
			if got := isProvisionalGreetingTitle(title); got != want {
				t.Fatalf("%q provisional %v", title, got)
			}
		}
	})
}

func TestMaybeAutoTitleUpgradesAProvisionalGreetingOnASubstantiveSecondTurn(t *testing.T) {
	titleTestHome(t)
	ctx := context.Background()
	db := newTitleTestDB(t, "cli", "sess-1")
	var mu sync.Mutex
	answers := []string{"Friendly greeting", "Debug scheduler failures"}
	swapTitleSeam(t, &titleCallLLM, func(context.Context, map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		answer := answers[0]
		answers = answers[1:]
		return titleResponse(map[string]any{"content": answer, "reasoning": nil}), nil
	})
	history := []*state.Message{{Role: "user", Content: "hi how are you"}}
	maybeAutoTitle(ctx, db, "sess-1", "hi how are you", autoTitleOptions{ConversationHistory: history})
	waitForTitleUpgrades(10 * time.Second)
	if src := titleSourceOf(t, db, "sess-1"); src != "derived" {
		t.Fatalf("greeting source %q", src)
	}
	history = append(history, &state.Message{Role: "assistant", Content: "Well, thanks."}, &state.Message{Role: "user", Content: "help me debug the scheduler"})
	maybeAutoTitle(ctx, db, "sess-1", "help me debug the scheduler", autoTitleOptions{ConversationHistory: history})
	waitForTitleUpgrades(10 * time.Second)
	if got := titleOf(t, db, "sess-1"); got == nil || *got != "Debug scheduler failures" || titleSourceOf(t, db, "sess-1") != "llm" {
		t.Fatalf("title %v", got)
	}
}

func TestMaybeAutoTitleDerivedTitleRetryCap(t *testing.T) {
	t.Run("test_a_placeholder_title_stops_retrying_after_the_third_turn", func(t *testing.T) {
		titleTestHome(t)
		ctx := context.Background()
		db := newTitleTestDB(t, "cli", "sess-1")
		if _, err := db.SetAutoTitleContext(ctx, "sess-1", "hi", "derived"); err != nil {
			t.Fatal(err)
		}
		history := []*state.Message{{Role: "user", Content: "turn 0"}, {Role: "user", Content: "turn 1"}, {Role: "user", Content: "turn 2"}}
		calls := fakeAutoTitleSession(t)
		maybeAutoTitle(ctx, db, "sess-1", "and now the real question", autoTitleOptions{ConversationHistory: history})
		waitForTitleUpgrades(10 * time.Second)
		if calls.Load() != 1 {
			t.Fatalf("turn 3 calls %d", calls.Load())
		}
		history = append(history, &state.Message{Role: "user", Content: "turn 3"})
		maybeAutoTitle(ctx, db, "sess-1", "and now the real question", autoTitleOptions{ConversationHistory: history})
		waitForTitleUpgrades(10 * time.Second)
		if calls.Load() != 1 {
			t.Fatalf("turn 4 calls %d", calls.Load())
		}
	})
}

func TestMaybeAutoTitleInstantTitleDeclinesANameCollision(t *testing.T) {
	titleTestHome(t)
	ctx := context.Background()
	db := newTitleTestDB(t, "cli", "taken")
	if _, err := db.SetSessionTitle(ctx, "taken", "hi"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "sess-1", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	fakeAutoTitleSession(t)
	maybeAutoTitle(ctx, db, "sess-1", "hi", autoTitleOptions{})
	if got := titleOf(t, db, "sess-1"); got != nil {
		t.Fatalf("title %q", *got)
	}
}

func TestAutoTitleDuplicateHandlingBackgroundStageNamesACollisionTheInstantStageDeclined(t *testing.T) {
	titleTestHome(t)
	ctx := context.Background()
	db := newTitleTestDB(t, "cli", "taken")
	if _, err := db.SetSessionTitle(ctx, "taken", "hi"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "sess-1", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	swapTitleSeam(t, &generateTitleFn, func(context.Context, string, generateTitleOptions) string { return "" })
	autoTitleSession(ctx, db, "sess-1", "hi", autoTitleSessionOptions{})
	if got := titleOf(t, db, "sess-1"); got == nil || *got != "hi #2" {
		t.Fatalf("title %v", got)
	}
}

func TestAutoTitleDuplicateHandlingDedupesDuplicateTitleViaLineage(t *testing.T) {
	titleTestHome(t)
	db := &fakeTitleDB{setAuto: []fakeSetAutoResult{{err: &pyint.Error{Kind: "ValueError", Msg: "in use"}}, {ok: true}}, next: "Debugging Import Error #2"}
	swapTitleSeam(t, &generateTitleFn, func(context.Context, string, generateTitleOptions) string { return "Debugging Import Error" })
	var seen []string
	autoTitleSession(context.Background(), db, "sess-1", "hi", autoTitleSessionOptions{TitleCallback: func(title, _ string) { seen = append(seen, title) }})
	if !reflect.DeepEqual(db.nextCalls, []string{"Debugging Import Error"}) {
		t.Fatalf("lineage calls %v", db.nextCalls)
	}
	if last := db.setAutoCalls[len(db.setAutoCalls)-1]; last[0] != "sess-1" || last[1] != "Debugging Import Error #2" {
		t.Fatalf("last set_auto_title %v", last)
	}
	if !reflect.DeepEqual(seen, []string{"Debugging Import Error #2"}) {
		t.Fatalf("seen %v", seen)
	}
}

func TestRuntimeValidatorBrokenValidatorFailsOpen(t *testing.T) {
	titleTestHome(t)
	calls := fakeTitleLLMContent(t, "Resilient Title")
	got := generateTitle(context.Background(), "question", generateTitleOptions{RuntimeValidator: func() bool { panic(errors.New("validator gone")) }})
	if got != "Resilient Title" || calls.Load() != 1 {
		t.Fatalf("title %q calls %d", got, calls.Load())
	}
}

const titleModelSwitchMarker = "[System: The active model for this chat has changed to " +
	"deepseek-v4-flash via provider 94mei. From this point forward, use " +
	"this runtime metadata when answering questions about what " +
	"model/provider is active.]"

func TestModelSwitchMarkerNotTitleableMarkerPrefixMatchesGatewayConstant(t *testing.T) {
	const gatewayPrefix = "[System: The active model for this chat has changed to "
	if !slices.Contains(titleMachinePrefixes, gatewayPrefix) || !strings.HasPrefix(titleModelSwitchMarker, gatewayPrefix) {
		t.Fatal("machine prefixes drifted from the model switch marker")
	}
}

func TestModelSwitchMarkerNotTitleableMarkerIsNotTitleable(t *testing.T) {
	if isTitleableUserMessage(titleModelSwitchMarker) {
		t.Fatal("marker is titleable")
	}
}

func TestModelSwitchMarkerNotTitleableUnrelatedSystemBracketTextStillTitleable(t *testing.T) {
	if !isTitleableUserMessage("[System: my own note] how do I ...") {
		t.Fatal("user bracket text is not titleable")
	}
}

func TestModelSwitchMarkerNotTitleableRealQuestionAfterMarkerStillTitles(t *testing.T) {
	titleTestHome(t)
	called := make(chan struct{})
	var once sync.Once
	swapTitleSeam(t, &autoTitleSessionFn, func(context.Context, any, string, string, autoTitleSessionOptions) { once.Do(func() { close(called) }) })
	history := []*state.Message{{Role: "user", Content: titleModelSwitchMarker}, {Role: "user", Content: "南京市秦淮区 小时级天气预报"}}
	maybeAutoTitle(context.Background(), &fakeTitleDB{}, "sess-1", "南京市秦淮区 小时级天气预报", autoTitleOptions{ConversationHistory: history})
	if !titleSignalled(called, 10*time.Second) {
		t.Fatal("auto_title never ran after marker")
	}
}

func TestModelSwitchMarkerNotTitleableInstantTitleSkipsMarkerUsesRealMessage(t *testing.T) {
	ctx := context.Background()
	db := &fakeTitleDB{}
	if got := applyInstantTitle(ctx, db, "sess-1", titleModelSwitchMarker, nil, nil); got != "" {
		t.Fatalf("marker titled %q", got)
	}
	if got := applyInstantTitle(ctx, db, "sess-1", "南京市秦淮区 小时级天气预报", nil, nil); got != "南京市秦淮区 小时级天气预报" {
		t.Fatalf("title %q", got)
	}
}
