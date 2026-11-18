package tty

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Truecolor SGR for a foreground and background pair
func sgr(fg, bg [3]int, extra string) string {
	s := fmt.Sprintf("\x1b[38;2;%d;%d;%d;48;2;%d;%d;%d", fg[0], fg[1], fg[2], bg[0], bg[1], bg[2])
	if extra != "" {
		s += ";" + extra
	}
	return s + "m"
}

var (
	cream   = [3]int{255, 248, 220}
	gold    = [3]int{184, 134, 11}
	night   = [3]int{5, 7, 10}
	surface = [3]int{10, 14, 19}
	rim     = [3]int{59, 81, 105}
	amber   = [3]int{255, 191, 0}
)

// Pads plain text with spaces to n columns, cutting what does not fit
func fit(s string, n int) string {
	if ansi.StringWidth(s) > n {
		return ansi.Truncate(s, n, "")
	}
	return s + strings.Repeat(" ", n-ansi.StringWidth(s))
}

// Chat screen in the toolkit's look with a bar, a boxed transcript and scrollbar, a status line and a composer
func chatScreen(w, h int, lines []string, status, composer string, scroll int) string {
	var b strings.Builder
	inner := w - 8
	bar := fit(" ▍1 session⋯  +", w-24) + fmt.Sprintf("%s ctx 1.2k/128k ", sgr(gold, night, "1")) + "│ fake"
	b.WriteString(sgr(cream, night, "") + fit(bar, w) + "\x1b[m\n")
	b.WriteString(sgr(cream, night, "") + strings.Repeat(" ", w) + "\x1b[m\n")
	b.WriteString(sgr(rim, night, "") + "  ┌─ ▍ session ─" + strings.Repeat("─", w-19) + "┐ " + "\x1b[m\n")
	view := h - 7
	start := max(0, len(lines)-view-scroll)
	for i := 0; i < view; i++ {
		text := ""
		if start+i < len(lines) {
			text = lines[start+i]
		}
		thumb := "│"
		if i >= view-3 {
			thumb = "┃"
		}
		b.WriteString(sgr(rim, night, "") + "  │" + sgr(cream, surface, "") + "  " + fit(text, inner-3) + sgr(amber, surface, "") + thumb + sgr(rim, night, "") + "│  \x1b[m\n")
	}
	b.WriteString(sgr(rim, night, "") + "  │" + sgr(gold, surface, "") + "  " + fit(status, inner-2) + sgr(rim, night, "") + "│  \x1b[m\n")
	b.WriteString(sgr(rim, night, "") + "  │" + sgr(gold, surface, "1") + " ❯ " + sgr(cream, surface, "") + fit(composer, inner-3) + sgr(rim, night, "") + "│  \x1b[m\n")
	b.WriteString(sgr(rim, night, "") + "  └─ enter send │ ctrl+g editor " + strings.Repeat("─", w-35) + "┘  \x1b[m")
	return b.String()
}

// Paragraphs the scripted model streams, fourteen characters at a time
func streamChunks(paragraphs int) []string {
	var text strings.Builder
	for i := 1; i <= paragraphs; i++ {
		if i > 1 {
			text.WriteString("\n\n")
		}
		fmt.Fprintf(&text, "Paragraph %d: %s", i, strings.Repeat("lorem ipsum dolor sit amet ", 12))
	}
	all := text.String()
	var out []string
	for i := 0; i < len(all); i += 14 {
		out = append(out, all[:min(i+14, len(all))])
	}
	return out
}

// Wraps streamed text into transcript lines of a width
func wrapText(text string, n int) []string {
	var out []string
	for _, para := range strings.Split(text, "\n") {
		for len(para) > n {
			cut := strings.LastIndex(para[:n], " ")
			if cut <= 0 {
				cut = n
			}
			out = append(out, para[:cut])
			para = strings.TrimLeft(para[cut:], " ")
		}
		out = append(out, para)
	}
	return out
}

func TestStreamedReplyMatchesFrames(t *testing.T) {
	const w, h = 120, 40
	g := newRig(t, w, h)
	chunks := streamChunks(30)
	faces := []string{"(｡•́︿•̀｡)", "(｡•́︿•̀｡)"}
	var first, streamBytes int
	for i, text := range chunks {
		lines := append([]string{"❯ write a long answer", ""}, wrapText(text, w-14)...)
		status := fmt.Sprintf("%s ruminating… · %d.%ds", faces[i%2], i/10, i%10)
		content := chatScreen(w, h, lines, status, "", 0)
		before := g.written
		g.show(screenView(content, 7, h-3))
		if i == 0 {
			first = g.written
		} else {
			streamBytes += g.written - before
		}
		g.check(content, fmt.Sprintf("chunk %d", i))
	}
	t.Logf("first frame %d bytes, %d streamed frames %d bytes", first, len(chunks)-1, streamBytes)
}

func TestResizeRepaintsEverything(t *testing.T) {
	g := newRig(t, 100, 30)
	lines := wrapText(strings.Repeat("resize me please ", 40), 80)
	content := chatScreen(100, 30, lines, "idle", "typed", 0)
	g.show(screenView(content, 9, 27))
	g.check(content, "initial")
	for _, sz := range [][2]int{{80, 24}, {132, 45}, {60, 20}, {100, 30}} {
		g.emu.Resize(sz[0], sz[1])
		g.r.Resize(sz[0], sz[1])
		content = chatScreen(sz[0], sz[1], lines, "idle", "typed", 0)
		g.show(screenView(content, 9, sz[1]-3))
		g.check(content, fmt.Sprintf("%dx%d", sz[0], sz[1]))
	}
}

func TestScrollingTranscriptUsesScrollOps(t *testing.T) {
	const w, h = 100, 30
	g := newRig(t, w, h)
	var lines []string
	for i := 0; i < 80; i++ {
		lines = append(lines, fmt.Sprintf("line %02d %s", i, strings.Repeat("abcdefghij", 6)))
	}
	for scroll := 0; scroll < 40; scroll += 3 {
		content := chatScreen(w, h, lines, "scrolling", "", scroll)
		before := g.written
		g.show(screenView(content, 7, h-3))
		g.check(content, fmt.Sprintf("scroll %d", scroll))
		if scroll > 0 && g.written-before > 1500 {
			t.Fatalf("scroll by 3 rows cost %d bytes, the scroll ops were not used", g.written-before)
		}
	}
}

func TestWideAndCombiningClusters(t *testing.T) {
	const w, h = 90, 24
	g := newRig(t, w, h)
	lines := []string{
		"CJK 漢字かなカナ한글 中文测试 全角ＡＢＣ end",
		"Emoji 😀 🎉 🚀 ⏳ ✅ 🧊 end",
		"VS16 ❤️ ☺️ ⚠️ ✔️ end",
		"ZWJ 👨‍👩‍👧‍👦 🧑‍🌾 end",
		"Flags 🇺🇸 🇯🇵 end",
		"Combining é ñ a̐ q̣̇ end",
		"Box ╭─╮│╰─╯ ▁▂▃▄ ⣿⠿ end",
	}
	content := chatScreen(w, h, lines, "(｡•́︿•̀｡) wide", "中文 ❤️", 0)
	g.show(screenView(content, 7, h-3))
	g.check(content, "wide")
	lines[0] = "CJK 漢字 changed かな end"
	content = chatScreen(w, h, lines, "(｡•́︿•̀｡) wide!", "中文 ❤️ x", 0)
	g.show(screenView(content, 7, h-3))
	g.check(content, "wide edit")
}
