package cliui

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Web search provider labels as the tools setup builds them
var searchProviders = []string{
	"Nous Subscription [subscription] — Managed web search and extract billed to your subscription  ★ via Nous Portal (login on select)",
	"Firecrawl Self-Hosted [free · self-hosted] — Run your own Firecrawl instance (Docker)",
	"Brave Search (Free) [free] — Free-tier API key — 2k queries/mo, search only.",
	"DuckDuckGo (ddgs) [free · no key · search only] — Search via the built-in DuckDuckGo client — no API key (pair with any extract provider)",
	"Exa · Free (keyless) [free · no key] — Semantic + neural web search with content extraction on Exa's anonymous free tier. Rate-limited under burst load.",
	"Exa · Paid (API key) [paid] — Semantic + neural web search with content extraction via the Exa SDK. Unthrottled, guaranteed service.",
	"Firecrawl [keyless/paid · optional gateway] — Full search + extract; supports keyless cloud, direct API, and Nous tool-gateway routing.",
	"Keenable · Free (keyless) [free · no key] — Independent web index for AI apps — fast search + page fetch on Keenable's anonymous free tier.",
	"Keenable · Paid (API key) [paid] — Independent web index for AI apps. Keyed access with higher limits and guaranteed service.",
	"OpenAI Native Web Search (Codex Responses) [native] — Search runs on the provider side (needs the Codex Responses transport + an openai-codex login); search only, extraction still uses another backend",
	"Parallel · Free (keyless) [free · no key] — Objective-tuned search + page extraction on Parallel's anonymous free tier. Rate-limited under burst load.",
	"Parallel · Paid (API key) [paid] — Objective-tuned search + parallel page extraction via the Parallel SDK. Unthrottled, guaranteed service.",
	"Perplexity [paid] — Perplexity Search API — ranked, date-stamped web results plus query-relevant page snippets for extract.",
	"SearXNG [free · self-hosted] — Free, privacy-respecting metasearch. Point SEARXNG_URL at your instance.",
	"Tavily [free · key optional] — Search + extract. Opt-in keyless; set TAVILY_API_KEY for higher limits.",
	"xAI Web Search (Grok) [paid] — Agentic web search via Grok's web_search tool — uses xAI Grok OAuth or XAI_API_KEY.",
	"Skip — keep defaults / configure later",
}

// Labels that strain a layout, a sparse state mark, an unbreakable word, wide characters and a bare long name
var awkwardLabels = []string{
	"Anthropic (Claude models via API key)  ← currently active",
	"OpenAI (GPT models through the Responses API)",
	"Custom endpoint - https://example.com/a/very-long/path-that-has-no-space-to-break-at/and-keeps-going/until-it-is-wider-than-most-terminals",
	"日本語のプロバイダー (東京リージョンで動作する高速な推論サービス)",
	"🎬 Video Analysis (video_analyze (requires a video model))",
	"Enter a custom model name that is long enough to need more than one row on a narrow terminal",
	"Skip",
}

// Screen rows of a screen text
func screenRows(s string) []string { return strings.Split(s, "\n") }

// Painted lines without color codes or trailing blanks
func plain(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = strings.TrimRight(ansi.Strip(line), " ")
	}
	return out
}

// Characters of a text other than spaces, in order of code point
func inked(text string) string {
	runes := []rune(strings.ReplaceAll(text, " ", ""))
	slices.Sort(runes)
	return string(runes)
}

// Draws a radiolist on a terminal of the size, returning the screen once it holds every wanted text
func drawn(t *testing.T, cols, rows int, title string, labels []string, selected int, keys string, want ...string) string {
	t.Helper()
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, cols, rows)
	done := make(chan struct{})
	go func() {
		r.c.CursesRadiolist(context.Background(), title, items(labels...), selected, cancelAt(-1))
		close(done)
	}()
	io.WriteString(pipeOut, keys)
	waitFor(t, r, func(s string) bool {
		for _, w := range want {
			if !strings.Contains(s, w) {
				return false
			}
		}
		return true
	})
	screen := r.text()
	io.WriteString(pipeOut, "\x1b")
	<-done
	return screen
}

func TestTextBreaksAtSpacesAndInsideWordsWiderThanTheRow(t *testing.T) {
	for _, tc := range []struct {
		text  string
		width int
		want  []string
	}{
		{"alpha beta gamma", 16, []string{"alpha beta gamma"}},
		{"alpha beta gamma", 11, []string{"alpha beta", "gamma"}},
		{"alpha beta gamma", 10, []string{"alpha beta", "gamma"}},
		{"alpha beta gamma", 5, []string{"alpha", "beta", "gamma"}},
		{"alpha  beta", 20, []string{"alpha  beta"}},
		{"a   b", 3, []string{"a", "b"}},
		{"abcdefghij", 4, []string{"abcd", "efgh", "ij"}},
		{"ab cdefghij k", 4, []string{"ab", "cdef", "ghij", "k"}},
		{"日本語のテキスト", 6, []string{"日本語", "のテキ", "スト"}},
		{"日本", 1, []string{"日", "本"}},
		{"keyless/paid · optional gateway", 23, []string{"keyless/paid · optional", "gateway"}},
		{"", 5, nil},
	} {
		m := measure(tc.text)
		if got := m.rows(tc.width); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q at %d broke into %q, want %q", tc.text, tc.width, got, tc.want)
		}
		if got := m.height(tc.width); got != len(tc.want) {
			t.Fatalf("%q at %d counted %d rows, want %d", tc.text, tc.width, got, len(tc.want))
		}
	}
	m := measure("🎬 Video Analysis")
	if m.width != ansi.StringWidth("🎬 Video Analysis") || m.word != 8 {
		t.Fatalf("measured width %d longest word %d", m.width, m.word)
	}
	if got := measure("a b  c").solid().rows(3); !reflect.DeepEqual(got, []string{"a b", "  c"}) {
		t.Fatalf("solid text broke into %q", got)
	}
}

func TestStyledRunsKeepTheirStyleAcrossABreak(t *testing.T) {
	r := keysRig("")
	r.env["COLORTERM"] = "truecolor"
	spans := []span{{"red words here", textStyle{color: "#ff0000"}}, {" ", textStyle{}}, {"bold tail", textStyle{bold: true}}}
	got := r.c.spanRows(measureSpans(spans), spans, 9)
	want := []string{"\x1b[38;2;255;0;0mred words\x1b[0m", "\x1b[38;2;255;0;0mhere\x1b[0m \x1b[1mbold\x1b[0m", "\x1b[1mtail\x1b[0m"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestWideTerminalDrawsEveryColumnWholeAndAligned(t *testing.T) {
	screen := drawn(t, 200, 40, "Select Search Provider", searchProviders, 1, "", "extraction still uses another backend")
	want := []string{
		"  ● Select Search Provider",
		"",
		"      Nous Subscription                           subscription                     Managed web search and extract billed to your subscription  ★ via Nous Portal (login on select)",
		"    ❯ Firecrawl Self-Hosted                       free · self-hosted               Run your own Firecrawl instance (Docker)",
		"      Brave Search (Free)                         free                             Free-tier API key — 2k queries/mo, search only.",
		"      DuckDuckGo (ddgs)                           free · no key · search only      Search via the built-in DuckDuckGo client — no API key (pair with any extract provider)",
		"      Exa · Free (keyless)                        free · no key                    Semantic + neural web search with content extraction on Exa's anonymous free tier. Rate-limited under burst load.",
		"      Exa · Paid (API key)                        paid                             Semantic + neural web search with content extraction via the Exa SDK. Unthrottled, guaranteed service.",
		"      Firecrawl                                   keyless/paid · optional gateway  Full search + extract; supports keyless cloud, direct API, and Nous tool-gateway routing.",
		"      Keenable · Free (keyless)                   free · no key                    Independent web index for AI apps — fast search + page fetch on Keenable's anonymous free tier.",
		"      Keenable · Paid (API key)                   paid                             Independent web index for AI apps. Keyed access with higher limits and guaranteed service.",
		"      OpenAI Native Web Search (Codex Responses)  native                           Search runs on the provider side (needs the Codex Responses transport + an openai-codex login); search only,",
		"                                                                                   extraction still uses another backend",
		"      Parallel · Free (keyless)                   free · no key                    Objective-tuned search + page extraction on Parallel's anonymous free tier. Rate-limited under burst load.",
		"      Parallel · Paid (API key)                   paid                             Objective-tuned search + parallel page extraction via the Parallel SDK. Unthrottled, guaranteed service.",
		"    ↓ 5 more",
		"",
		"    ↑↓ move  ·  enter select  ·  esc cancel",
	}
	if got := screenRows(screen); !reflect.DeepEqual(got, want) {
		t.Fatalf("screen:\n%s", screen)
	}
}

func TestColumnsNarrowAndWrapBeforeAnyTextIsCut(t *testing.T) {
	screen := drawn(t, 120, 40, "Select Search Provider", searchProviders, 0, "", "plus query-relevant page snippets for extract.")
	for _, want := range []string{
		"    ❯ Nous Subscription          subscription                 Managed web search and extract billed to your\n" +
			"                                                              subscription  ★ via Nous Portal (login on select)\n",
		"      Firecrawl                  keyless/paid · optional      Full search + extract; supports keyless cloud, direct\n" +
			"                                 gateway                      API, and Nous tool-gateway routing.\n",
		"      OpenAI Native Web Search   native                       Search runs on the provider side (needs the Codex\n" +
			"      (Codex Responses)                                       Responses transport + an openai-codex login); search\n" +
			"                                                              only, extraction still uses another backend\n",
	} {
		if !strings.Contains(screen, want) {
			t.Fatalf("missing rows:\n%s\nscreen:\n%s", want, screen)
		}
	}
	if strings.Contains(screen, "…") {
		t.Fatalf("text was cut:\n%s", screen)
	}
}

func TestNarrowTerminalStacksExplanationsUnderTheirNames(t *testing.T) {
	screen := drawn(t, 60, 24, "Select Search Provider", searchProviders, 1, "", "enter select")
	want := strings.Join([]string{
		"  ● Select Search Provider",
		"",
		"      Nous Subscription  subscription",
		"        Managed web search and extract billed to your",
		"        subscription  ★ via Nous Portal (login on select)",
		"    ❯ Firecrawl Self-Hosted  free · self-hosted",
		"        Run your own Firecrawl instance (Docker)",
		"      Brave Search (Free)  free",
		"        Free-tier API key — 2k queries/mo, search only.",
		"      DuckDuckGo (ddgs)  free · no key · search only",
		"        Search via the built-in DuckDuckGo client — no API",
		"        key (pair with any extract provider)",
		"      Exa · Free (keyless)  free · no key",
		"        Semantic + neural web search with content",
		"        extraction on Exa's anonymous free tier.",
		"        Rate-limited under burst load.",
		"      Exa · Paid (API key)  paid",
		"        Semantic + neural web search with content",
		"        extraction via the Exa SDK. Unthrottled, guaranteed",
		"        service.",
		"    ↓ 11 more",
		"",
		"    ↑↓ move  ·  enter select  ·  esc cancel",
	}, "\n")
	if screen != want {
		t.Fatalf("screen:\n%s", screen)
	}
}

func TestSparseStateMarkLeadsItsExplanation(t *testing.T) {
	screen := drawn(t, 100, 20, "Provider", []string{
		"Anthropic (Claude models via API key)  ← currently active",
		"OpenAI (GPT models)",
		"Mistral (European models)",
		"Groq (fast inference)",
	}, 0, "", "fast inference")
	for _, want := range []string{
		"    ❯ Anthropic  currently active  Claude models via API key\n",
		"      OpenAI     GPT models\n",
		"      Groq       fast inference\n",
	} {
		if !strings.Contains(screen, want) {
			t.Fatalf("missing %q:\n%s", want, screen)
		}
	}
}

func TestEveryRowKeepsAllItsTextWithinTheTerminalAtAnyWidth(t *testing.T) {
	labels := append(append([]string{}, searchProviders...), awkwardLabels...)
	rows := append(items(labels...), RadioItem{{Text: "★ ", Style: "yellow"}, {Text: "styled-model-name   "}, {Text: " $1.25   $10.00", Style: "dim"}, {Text: "  ← currently in use"}})
	for cols := 15; cols <= 240; cols++ {
		for _, multi := range []bool{false, true} {
			r := newRig(strings.NewReader(""), cols, 40)
			p := r.c.newListPrompt("Pick", rows, 0, map[int]string{0: "A section heading that is itself longer than a narrow terminal is wide", 17: "More"})
			p.multi = multi
			lay := p.layout(bodyWidth(cols))
			for i, row := range p.rows {
				want := row.heading + RadioItemPlain(row.item)
				if row.primary != "" {
					want = row.primary + row.tag + row.secondary
				}
				if multi && row.index >= 0 {
					want += "[]"
				}
				lines := plain(p.rowLines(row, false, lay))
				for _, line := range lines {
					if w := ansi.StringWidth(line); w > cols-1 {
						t.Fatalf("%d columns, multi %v: row %d line is %d wide:\n%s", cols, multi, i, w, strings.Join(lines, "\n"))
					}
				}
				if got := inked(strings.Join(lines, "")); got != inked(want) {
					t.Fatalf("%d columns, multi %v: row %d lost or gained text:\n%s", cols, multi, i, strings.Join(lines, "\n"))
				}
				if len(lines) != lay.heights[i] {
					t.Fatalf("%d columns: row %d drew %d lines, laid out as %d", cols, i, len(lines), lay.heights[i])
				}
			}
		}
	}
}

func TestEveryLineOfEveryChoiceCanBeScrolledIntoViewOnAnyTerminal(t *testing.T) {
	labels := append(append([]string{}, searchProviders...), awkwardLabels...)
	for _, size := range [][2]int{{15, 8}, {17, 8}, {24, 9}, {30, 10}, {40, 16}, {60, 24}, {80, 24}, {100, 30}, {120, 40}, {160, 50}, {200, 50}, {240, 12}} {
		cols, height := size[0], size[1]
		r := newRig(strings.NewReader(""), cols, height)
		p := r.c.newListPrompt("Select Search Provider", items(labels...), 0, map[int]string{0: "Search", 17: "Other"})
		maxRows := height - 1
		seen := map[int]map[string]bool{}
		for range 2000 {
			frame := plain(p.frame(maxRows, cols))
			if len(frame) > maxRows {
				t.Fatalf("%dx%d: frame of %d lines:\n%s", cols, height, len(frame), strings.Join(frame, "\n"))
			}
			if seen[p.cursor] == nil {
				seen[p.cursor] = map[string]bool{}
			}
			for _, line := range frame {
				if w := ansi.StringWidth(line); w > cols-1 {
					t.Fatalf("%dx%d: line is %d wide: %q", cols, height, w, line)
				}
				if strings.Contains(line, "…") {
					t.Fatalf("%dx%d: cut text: %q", cols, height, line)
				}
				seen[p.cursor][line] = true
			}
			if p.cursor == len(p.rows)-1 && p.rowBelow == 0 {
				break
			}
			p.step(1)
		}
		for i, row := range p.rows {
			if row.index < 0 {
				continue
			}
			for _, line := range plain(p.rowLines(row, true, p.laid)) {
				if !seen[i][line] {
					t.Fatalf("%dx%d: choice %d never showed %q", cols, height, row.index, line)
				}
			}
		}
	}
}

func TestChoiceTallerThanTheViewScrollsLineByLine(t *testing.T) {
	keys := ""
	for step, want := range []string{
		"    ↑ 1 more\n    ❯ Firecrawl\n      Self-Hosted\n        free ·\n    ↓ 5 lines",
		"    ↑ 1 line\n      Self-Hosted\n        free ·\n        self-hosted\n    ↓ 4 lines",
		"    ↑ 2 lines\n        free ·\n        self-hosted\n        Run your own\n    ↓ 3 lines",
		"    ↑ 3 lines\n        self-hosted\n        Run your own\n        Firecrawl\n    ↓ 2 lines",
		"    ↑ 4 lines\n        Run your own\n        Firecrawl\n        instance\n    ↓ 1 line",
		"    ↑ 5 lines\n        Firecrawl\n        instance\n        (Docker)\n    ↓ 15 more",
		"    ↑ 2 more\n    ❯ Brave Search\n      (Free)\n        free\n    ↓ 4 lines",
	} {
		screen := drawn(t, 24, 9, "Select Search Provider", searchProviders, 1, keys, want)
		if strings.Contains(screen, "…") {
			t.Fatalf("step %d cut text:\n%s", step, screen)
		}
		keys += "\x1b[B"
	}
	drawn(t, 24, 9, "Select Search Provider", searchProviders, 1, "\x1b[B\x1b[B\x1b[A\x1b[A\x1b[A", "    Provider\n\n    ❯ Nous Subscription\n")
}

func TestSectionHeadingReturnsWithItsFirstChoice(t *testing.T) {
	labels := make([]string, 30)
	for i := range labels {
		labels[i] = fmt.Sprintf("choice %02d", i)
	}
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 80, 30)
	done := make(chan int, 1)
	go func() {
		got, _ := r.c.CursesRadiolist(context.Background(), "Models", items(labels...), 0,
			RadiolistOptions{CancelReturns: new(int), Headings: map[int]string{0: "Fast", 20: "Large"}})
		done <- got
	}()
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "    Fast\n    ❯ choice 00") })
	io.WriteString(pipeOut, strings.Repeat("\x1b[B", 20))
	waitFor(t, r, func(s string) bool {
		return strings.Contains(s, "    Large\n    ❯ choice 20") && !strings.Contains(s, "Fast")
	})
	io.WriteString(pipeOut, strings.Repeat("\x1b[A", 20))
	waitFor(t, r, func(s string) bool {
		return strings.Contains(s, "  ● Models\n\n    Fast\n    ❯ choice 00")
	})
	io.WriteString(pipeOut, "\r")
	if got := <-done; got != 0 {
		t.Fatalf("picked %d", got)
	}
}

func TestDescriptionStatusAndFilterWrapOnANarrowTerminal(t *testing.T) {
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 40, 30)
	done := make(chan struct{})
	go func() {
		r.c.CursesRadiolist(context.Background(), "Pick a model for the main agent loop", items("alpha", "beta"), 0, RadiolistOptions{
			CancelReturns: new(int), Searchable: true,
			Description: "Prices are per million tokens and ★ marks a model on sale this week\n\n          aligned under the rows and long enough to wrap twice over"})
		close(done)
	}()
	waitFor(t, r, func(s string) bool {
		return strings.Contains(s, strings.Join([]string{
			"  ● Pick a model for the main agent",
			"    loop",
			"    Prices are per million tokens and ★",
			"    marks a model on sale this week",
			"",
			"           aligned under the rows and",
			"           long enough to wrap twice",
			"           over",
			"",
			"    ❯ alpha",
			"      beta",
			"",
			"    ↑↓ move  ·  enter select",
			"    / filter  ·  esc cancel",
		}, "\n"))
	})
	io.WriteString(pipeOut, "/a query that is far too long for one row of this terminal")
	waitFor(t, r, func(s string) bool {
		return strings.Contains(s, "    / a query that is far too long fo\n      r one row of this terminal\n    No matches")
	})
	io.WriteString(pipeOut, "\x1b")
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "/ filter") })
	io.WriteString(pipeOut, "\x1b")
	<-done
	pipeIn, pipeOut = io.Pipe()
	r = newRig(pipeIn, 40, 30)
	done = make(chan struct{})
	go func() {
		r.c.CursesChecklist(context.Background(), "Tools", []string{"web", "image"}, nil, nil,
			func(map[int]bool) string { return "Estimated context cost of the selected tools is 12,400 tokens" })
		close(done)
	}()
	waitFor(t, r, func(s string) bool {
		return strings.Contains(s, "    Estimated context cost of the\n    selected tools is 12,400 tokens\n    ↑↓ move  ·  space toggle\n    enter confirm  ·  esc cancel")
	})
	io.WriteString(pipeOut, "\x1b")
	<-done
}

func TestLongDefaultWrapsInTheFieldItsHintAndItsAnswer(t *testing.T) {
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 40, 20)
	done := make(chan string, 1)
	go func() {
		got, _ := r.c.LineInputDefault("Base URL: ", "Base URL", "https://inference.example.com/v1/chat/completions")
		done <- got
	}()
	waitFor(t, r, func(s string) bool {
		return strings.Contains(s, strings.Join([]string{
			"  ● Base URL",
			"    ❯  https://inference.example.com/v1",
			"       /chat/completions",
			"    enter keeps",
			"    https://inference.example.com/v1/ch",
			"    at/completions",
		}, "\n"))
	})
	io.WriteString(pipeOut, "\r")
	<-done
	if got := r.text(); got != "  › Base URL\n    https://inference.example.com/v1/ch\n    at/completions" {
		t.Fatalf("answer rows:\n%q", got)
	}
	secret := newRig(strings.NewReader("0123456789abcdef\r"), 30, 20)
	secret.c.SecretLine("A long provider API key: ", "*")
	if got := secret.text(); got != "  › A long provider API key\n    •••••••••••• (16\n    characters)" {
		t.Fatalf("secret answer rows:\n%q", got)
	}
}

func TestBannerTaglineWrapsOnANarrowTerminal(t *testing.T) {
	r := newRig(strings.NewReader(""), 24, 10)
	r.c.Flow().Banner(nil, "icebath", "Set up providers, tools and the gateway")
	if got := r.text(); got != "\n  icebath\n\n  Set up providers,\n  tools and the gateway" {
		t.Fatalf("banner:\n%q", got)
	}
}

func TestDescriptionColumnHeadersLineUpWithTheRowColumnsUnderThem(t *testing.T) {
	row := func(name, in, out string) RadioItem {
		return RadioItem{{Text: fmt.Sprintf("%-16s", name)}, {Text: fmt.Sprintf(" %6s  %6s", in, out)}}
	}
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 100, 30)
	done := make(chan struct{})
	go func() {
		r.c.CursesRadiolist(context.Background(), "Select default model", []RadioItem{row("claude-fable-5", "$3.00", "$15.00"), row("claude-haiku-5", "$0.80", "$4.00")}, 0,
			RadiolistOptions{CancelReturns: new(int), Description: fmt.Sprintf("%5s%16s %6s  %6s  $/Mtok\n     priced per million tokens", "", "", "In", "Out")})
		close(done)
	}()
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "claude-haiku-5") })
	want := strings.Join([]string{
		"  ● Select default model",
		"                           In     Out  $/Mtok",
		"      priced per million tokens",
		"",
		"    ❯ claude-fable-5    $3.00  $15.00",
		"      claude-haiku-5    $0.80   $4.00",
	}, "\n")
	if got := r.text(); !strings.HasPrefix(got, want) {
		t.Fatalf("screen:\n%s", got)
	}
	io.WriteString(pipeOut, "\r")
	<-done
}
