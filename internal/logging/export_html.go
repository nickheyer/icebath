package logging

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

//go:embed export.html
var htmlTemplate string

// Line icons the HTML export draws its roles, tool calls, reasoning and controls with
const (
	iconUser     = `<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide lucide-user"><path d="M19 21v-2a4 4 0 0 0-4-4H9a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/></svg>`
	iconBot      = `<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide lucide-bot"><path d="M12 8V4H8"/><rect width="16" height="12" x="4" y="8" rx="2"/><path d="M2 14h2"/><path d="M20 14h2"/><path d="M15 13v2"/><path d="M9 13v2"/></svg>`
	iconTerminal = `<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide lucide-terminal"><polyline points="4 17 10 11 4 5"/><line x1="12" x2="20" y1="19" y2="19"/></svg>`
	iconWrench   = `<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide lucide-wrench"><path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z"/></svg>`
	iconSparkles = `<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide lucide-sparkles"><path d="m12 3-1.912 5.813a2 2 0 0 1-1.275 1.275L3 12l5.813 1.912a2 2 0 0 1 1.275 1.275L12 21l1.912-5.813a2 2 0 0 1 1.275-1.275L21 12l-5.813-1.912a2 2 0 0 1-1.275-1.275L12 3Z"/><path d="M5 3v4"/><path d="M19 17v4"/><path d="M3 5h4"/><path d="M17 19h4"/></svg>`
	iconSearch   = `<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide lucide-search"><circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/></svg>`
	iconShield   = `<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide lucide-shield"><path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.5 3.8 17 5 19 5a1 1 0 0 1 1 1z"/></svg>`
	iconBrand    = `<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="#FFD700" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/><path d="m9 12 2 2 4-4"/></svg>`
)

// Chevron icon carrying the class the collapsible headers rotate
const chevronHTML = `<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="chevron lucide lucide-chevron-right"><path d="m9 18 6-6-6-6"/></svg>`

// Role badge icon of a role, the terminal icon for any role but user, assistant and system
func roleIcon(role *jv) string {
	if role.kind == 's' {
		switch role.str {
		case "user":
			return iconUser
		case "assistant":
			return iconBot
		case "system":
			return iconShield
		}
	}
	return iconTerminal
}

// Text with ampersands, angle brackets and both quotes written as HTML entities
func escapeHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}

// Local time of a stored timestamp in whole seconds, N/A for an unset, zero or corrupt cell
func htmlTime(v *jv) string {
	if !v.truthy() {
		return "N/A"
	}
	ts, ok := coerceEpoch(v, "", "timestamp")
	if !ok || ts == 0 {
		return "N/A"
	}
	return epochTime(ts).Local().Format("2006-01-02 15:04:05")
}

// Display text of message content, list parts flattened and images shown as an attachment marker
func htmlContent(content *jv) string {
	if !content.truthy() {
		return ""
	}
	if content.kind != 'a' {
		return content.display()
	}
	var parts []string
	for _, p := range content.vals {
		switch {
		case p.kind != 'o':
			parts = append(parts, p.display())
		case p.get("type").display() == "text":
			parts = append(parts, valueOr(p.get("text"), "").display())
		case p.get("type").display() == "image_url":
			parts = append(parts, "[Image Attachment]")
		}
	}
	return strings.Join(parts, "\n")
}

// Member value when the member is present, null included, else the string def
func valueOr(v *jv, def string) *jv {
	if v == nil {
		return jvString(def)
	}
	return v
}

// Chevron headed block that folds open, of a kind with its icon, title and inner markup
func collapsible(kind, icon, title, inner, indent, outerClass string) string {
	if outerClass == "" {
		outerClass = kind
	}
	return "\n" + indent + `<div class="` + outerClass + `">` + "\n" +
		indent + `    <div class="` + kind + `-header">` + "\n" +
		indent + "        " + chevronHTML + "\n" +
		indent + "        " + icon + " " + title + "\n" +
		indent + "    </div>\n" +
		indent + `    <div class="` + kind + `-content">` + "\n" +
		indent + "        " + inner + "\n" +
		indent + "    </div>\n" +
		indent + "</div>\n" + indent
}

// Seconds of the staggered fade in delay written the way the reference prints the number
func delaySeconds(i int) string {
	text := strconv.FormatFloat(min(float64(i)*0.05, 1.0), 'f', -1, 64)
	if !strings.Contains(text, ".") {
		text += ".0"
	}
	return text
}

// Cards of every message but session metadata, with their tool calls, content and reasoning
func messagesHTML(messages []*jv) string {
	var cards []string
	for i, m := range messages {
		role := valueOr(m.get("role"), "unknown")
		roleText := role.display()
		if roleText == "session_meta" {
			continue
		}
		var class strings.Builder
		for _, r := range strings.ToLower(roleText) {
			if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '-' || r == '_' {
				class.WriteRune(r)
			} else {
				class.WriteByte('-')
			}
		}
		delay := ""
		if i < 10 {
			delay = ` style="animation-delay: ` + delaySeconds(i) + `s"`
		}
		card := `<div class="message message-` + class.String() + ` active"` + delay + `>` +
			`  <div class="message-header">` +
			`    <div class="role-badge">` + chevronHTML + " " + roleIcon(role) + " " + escapeHTML(roleText) + `</div>` +
			`    <div class="timestamp">` + htmlTime(valueOr(m.get("timestamp"), "")) + `</div>` +
			`  </div>` +
			`  <div class="message-body">`
		if calls := m.get("tool_calls"); calls.truthy() && calls.kind == 'a' {
			for _, tc := range calls.vals {
				fn := tc.get("function")
				card += collapsible("tool-call", iconWrench, "Tool Call: "+escapeHTML(valueOr(fn.get("name"), "unknown").display()),
					"<pre><code>"+escapeHTML(valueOr(fn.get("arguments"), "{}").display())+"</code></pre>", strings.Repeat(" ", 16), "")
			}
		}
		if content := htmlContent(m.get("content")); content != "" {
			escaped := escapeHTML(content)
			if roleText == "tool" {
				escaped = "<pre><code>" + escaped + "</code></pre>"
			}
			card += `  <div class="content">` + escaped + `</div>`
		}
		if reasoning := first(m.get("reasoning"), m.get("reasoning_content")); reasoning.truthy() {
			card += collapsible("reasoning", iconSparkles, "Reasoning",
				`<div class="content">`+escapeHTML(reasoning.display())+`</div>`, strings.Repeat(" ", 12), "")
		}
		cards = append(cards, card+"  </div></div>")
	}
	return strings.Join(cards, "\n")
}

// Text with every byte outside the unreserved set percent encoded, slashes included
func percentEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("_.-~", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// First n runes of s
func runePrefix(s string, n int) string {
	r := []rune(s)
	return string(r[:min(n, len(r))])
}

// Sidebar entry linking to one session view, its title cut to fifty characters
func sidebarItemHTML(s *jv) string {
	sid := valueOr(s.get("id"), "N/A").display()
	title := first(s.get("title"), s.get("preview"), jvString("Untitled Session")).display()
	if len([]rune(title)) > 50 {
		title = runePrefix(title, 47) + "..."
	}
	date, _, _ := strings.Cut(htmlTime(valueOr(s.get("started_at"), "")), " ")
	return `
            <a class="session-item" data-id="` + escapeHTML(sid) + `" href="#` + percentEncode(sid) + `">
                <div class="session-item-title">` + escapeHTML(title) + `</div>
                <div class="session-item-meta">
                    <span>` + escapeHTML(runePrefix(sid, 8)) + `</span>
                    <span>` + date + `</span>
                </div>
            </a>
            `
}

// Sidebar with the brand, the session search and one entry per session
func sidebarHTML(sessions []*jv) string {
	var items strings.Builder
	for _, s := range sessions {
		items.WriteString(sidebarItemHTML(s))
	}
	return `
        <aside class="sidebar">
            <div class="sidebar-header">
                <div class="sidebar-brand">
                    ` + iconBrand + ` icebath History
                </div>
                <div class="search-container">
                    ` + iconSearch + `
                    <input type="text" id="session-search" placeholder="Search sessions...">
                </div>
            </div>
            <div class="session-list">
                ` + items.String() + `
            </div>
        </aside>
        `
}

// View of one session with its header, metadata, system prompt and messages
func sessionViewHTML(s *jv, multi bool) string {
	sid := escapeHTML(valueOr(s.get("id"), "N/A").display())
	system := ""
	if prompt := s.get("system_prompt"); prompt.truthy() {
		system = collapsible("system-prompt", iconShield, "System Prompt (Persona)",
			`<div class="content">`+escapeHTML(prompt.display())+`</div>`, strings.Repeat(" ", 12), "system-prompt-section active")
	}
	class := "session-view active"
	if multi {
		class = "session-view"
	}
	return `
        <div class="` + class + `" id="view-` + sid + `">
            <header class="fade-in">
                <h1>` + escapeHTML(first(s.get("title"), jvString("icebath Session")).display()) + `</h1>
                <div class="meta">
                    <div class="meta-item"><strong>ID:</strong> ` + sid + `</div>
                    <div class="meta-item"><strong>Model:</strong> ` + escapeHTML(first(s.get("model"), jvString("Unknown")).display()) + `</div>
                    <div class="meta-item"><strong>Started:</strong> ` + htmlTime(valueOr(s.get("started_at"), "")) + `</div>
                </div>
                ` + system + `
            </header>
            <main>
                ` + messagesHTML(messagesOf(s)) + `
            </main>
        </div>
        `
}

// Standalone HTML page of the exported sessions, a sidebar switching between them when there are several
func RenderHTML(sessions [][]byte) (string, error) {
	docs, err := parseDocs(sessions)
	if err != nil {
		return "", err
	}
	if len(docs) == 0 {
		return "<html><body><h1>No sessions to export.</h1></body></html>", nil
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	multi := len(docs) > 1
	title := "icebath Session Export"
	sidebar, margin, layout := "", "0", "layout-single"
	if multi {
		sidebar, margin, layout = sidebarHTML(docs), "var(--sidebar-width)", "layout-multi"
	} else {
		title = escapeHTML(first(docs[0].get("title"), jvString("icebath Session")).display())
	}
	views := make([]string, len(docs))
	for i, s := range docs {
		views[i] = sessionViewHTML(s, multi)
	}
	return strings.NewReplacer(
		"{page_title}", title,
		"{sidebar_html}", sidebar,
		"{sessions_html}", strings.Join(views, "\n"),
		"{main_margin}", margin,
		"{layout_class}", layout,
		"{generated_at}", time.Now().Format("2006-01-02 15:04:05"),
		"{script_nonce}", base64.RawURLEncoding.EncodeToString(nonce),
	).Replace(htmlTemplate), nil
}
