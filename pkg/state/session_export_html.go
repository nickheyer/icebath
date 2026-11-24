package state

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	templateutil "github.com/nickheyer/icebath/pkg/utils/template"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

const (
	iconUser         = "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"16\" height=\"16\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"2\" stroke-linecap=\"round\" stroke-linejoin=\"round\" class=\"lucide lucide-user\"><path d=\"M19 21v-2a4 4 0 0 0-4-4H9a4 4 0 0 0-4 4v2\"/><circle cx=\"12\" cy=\"7\" r=\"4\"/></svg>"
	iconBot          = "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"16\" height=\"16\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"2\" stroke-linecap=\"round\" stroke-linejoin=\"round\" class=\"lucide lucide-bot\"><path d=\"M12 8V4H8\"/><rect width=\"16\" height=\"12\" x=\"4\" y=\"8\" rx=\"2\"/><path d=\"M2 14h2\"/><path d=\"M20 14h2\"/><path d=\"M15 13v2\"/><path d=\"M9 13v2\"/></svg>"
	iconTerminal     = "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"16\" height=\"16\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"2\" stroke-linecap=\"round\" stroke-linejoin=\"round\" class=\"lucide lucide-terminal\"><polyline points=\"4 17 10 11 4 5\"/><line x1=\"12\" x2=\"20\" y1=\"19\" y2=\"19\"/></svg>"
	iconWrench       = "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"16\" height=\"16\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"2\" stroke-linecap=\"round\" stroke-linejoin=\"round\" class=\"lucide lucide-wrench\"><path d=\"M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z\"/></svg>"
	iconSparkles     = "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"16\" height=\"16\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"2\" stroke-linecap=\"round\" stroke-linejoin=\"round\" class=\"lucide lucide-sparkles\"><path d=\"m12 3-1.912 5.813a2 2 0 0 1-1.275 1.275L3 12l5.813 1.912a2 2 0 0 1 1.275 1.275L12 21l1.912-5.813a2 2 0 0 1 1.275-1.275L21 12l-5.813-1.912a2 2 0 0 1-1.275-1.275L12 3Z\"/><path d=\"M5 3v4\"/><path d=\"M19 17v4\"/><path d=\"M3 5h4\"/><path d=\"M17 19h4\"/></svg>"
	iconChevronRight = "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"14\" height=\"14\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"2\" stroke-linecap=\"round\" stroke-linejoin=\"round\" class=\"lucide lucide-chevron-right\"><path d=\"m9 18 6-6-6-6\"/></svg>"
	iconSearch       = "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"16\" height=\"16\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"2\" stroke-linecap=\"round\" stroke-linejoin=\"round\" class=\"lucide lucide-search\"><circle cx=\"11\" cy=\"11\" r=\"8\"/><path d=\"m21 21-4.3-4.3\"/></svg>"
	iconShield       = "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"16\" height=\"16\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"2\" stroke-linecap=\"round\" stroke-linejoin=\"round\" class=\"lucide lucide-shield\"><path d=\"M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.5 3.8 17 5 19 5a1 1 0 0 1 1 1z\"/></svg>"
	iconIcebath      = "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"24\" height=\"24\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"#e3122b\" stroke-width=\"2\" stroke-linecap=\"round\" stroke-linejoin=\"round\"><path d=\"M21 8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16Z\"/><path d=\"m3.3 7 8.7 5 8.7-5\"/><path d=\"M12 22V12\"/></svg>"
)

//go:embed session_export_template.html
var htmlExportTemplate string

var (
	htmlRoleIcons = map[string]string{"user": iconUser, "assistant": iconBot, "system": iconShield}
	htmlChevron   = strings.ReplaceAll(iconChevronRight, `class="`, `class="chevron `)
)

// Text of a value with the five HTML special characters escaped
func escapeHTML(text any) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(anyutil.String(text))
}

// Local date and time of a stored timestamp, N/A when unset or corrupt
func htmlFormatTimestamp(ts any) string {
	if !anyutil.IsTrue(ts) {
		return "N/A"
	}
	epoch, ok := timeutil.StoredEpoch(ts, "", "timestamp")
	if !ok || epoch == 0 {
		return "N/A"
	}
	return timeutil.FromEpoch(epoch).Format(time.DateTime)
}

// Display text of a message content, the text parts of a multimodal list joined and images named
func htmlContentText(content any) (any, error) {
	list, ok := exportJSONValue(content).([]any)
	if !ok {
		return exportJSONValue(content), nil
	}
	var parts []string
	for _, part := range list {
		obj, isObject := part.(*jsonutil.Object)
		if !isObject {
			if m, isMap := part.(map[string]any); isMap {
				obj, isObject = jsonutil.NewObject(), true
				for k, v := range m {
					obj.Set(k, v)
				}
			}
		}
		switch {
		case !isObject:
			parts = append(parts, anyutil.String(part))
		case rowValue(obj, "type") == "text":
			text, present := obj.Get("text")
			if !present {
				text = ""
			}
			s, isString := text.(string)
			if !isString {
				return nil, &errorutil.Error{Kind: "TypeError", Msg: "sequence item " + anyutil.String(len(parts)) + ": expected str instance, " + anyutil.Kind(text) + " found"}
			}
			parts = append(parts, s)
		case rowValue(obj, "type") == "image_url":
			parts = append(parts, "[Image Attachment]")
		}
	}
	return strings.Join(parts, "\n"), nil
}

// Collapsible block with a chevron header, the markup of a tool call, a reasoning or a system prompt section
func htmlCollapsible(kind, icon, title, inner, indent, outerClass string) string {
	if outerClass == "" {
		outerClass = kind
	}
	return "\n" + indent + `<div class="` + outerClass + `">` + "\n" +
		indent + `    <div class="` + kind + `-header">` + "\n" +
		indent + "        " + htmlChevron + "\n" +
		indent + "        " + icon + " " + title + "\n" +
		indent + "    </div>\n" +
		indent + `    <div class="` + kind + `-content">` + "\n" +
		indent + "        " + inner + "\n" +
		indent + "    </div>\n" +
		indent + "</div>\n" +
		indent
}

// Value of a key that may be absent, the fallback when it is
func htmlGetOr(row *jsonutil.Object, key string, fallback any) any {
	if row != nil {
		if v, ok := row.Get(key); ok {
			return v
		}
	}
	return fallback
}

// Message blocks of a session, tool calls and reasoning folded, every stored text escaped
func generateMessagesHTML(messages []*jsonutil.Object) (string, error) {
	var blocks []string
	for i, msg := range messages {
		role := htmlGetOr(msg, "role", "unknown")
		if role == "session_meta" {
			continue
		}
		raw := rowValue(msg, "content")
		if !anyutil.IsTrue(raw) {
			raw = ""
		}
		content, err := htmlContentText(raw)
		if err != nil {
			return "", err
		}
		var roleClass strings.Builder
		for _, c := range strings.ToLower(anyutil.String(role)) {
			if (unicode.IsLetter(c) || unicode.IsNumber(c)) || c == '-' || c == '_' {
				roleClass.WriteRune(c)
			} else {
				roleClass.WriteByte('-')
			}
		}
		delayStyle := ""
		if i < 10 {
			delayStyle = ` style="animation-delay: ` + strconv.FormatFloat(min(float64(i)*0.05, 1.0), 'f', -1, 64) + `s"`
		}
		icon := iconTerminal
		if name, ok := role.(string); ok {
			if known, found := htmlRoleIcons[name]; found {
				icon = known
			}
		}
		html := `<div class="message message-` + roleClass.String() + ` active"` + delayStyle + `>` +
			`  <div class="message-header">` +
			`    <div class="role-badge">` + htmlChevron + " " + icon + " " + escapeHTML(role) + `</div>` +
			`    <div class="timestamp">` + htmlFormatTimestamp(htmlGetOr(msg, "timestamp", 0)) + `</div>` +
			`  </div>` +
			`  <div class="message-body">`
		if calls := exportJSONValue(rowValue(msg, "tool_calls")); anyutil.IsTrue(calls) {
			list, _ := calls.([]any)
			for _, tc := range list {
				fn, _ := htmlGetOr(asExportObject(tc), "function", jsonutil.NewObject()).(*jsonutil.Object)
				html += htmlCollapsible("tool-call", iconWrench, "Tool Call: "+escapeHTML(htmlGetOr(fn, "name", "unknown")),
					"<pre><code>"+escapeHTML(htmlGetOr(fn, "arguments", "{}"))+"</code></pre>", strings.Repeat(" ", 16), "")
			}
		}
		if anyutil.IsTrue(content) {
			escaped := escapeHTML(content)
			if role == "tool" || role == shellLineRole {
				escaped = "<pre><code>" + escaped + "</code></pre>"
			}
			html += `  <div class="content">` + escaped + `</div>`
		}
		reasoning := rowValue(msg, "reasoning")
		if !anyutil.IsTrue(reasoning) {
			reasoning = rowValue(msg, "reasoning_content")
		}
		if anyutil.IsTrue(reasoning) {
			html += htmlCollapsible("reasoning", iconSparkles, "Reasoning", `<div class="content">`+escapeHTML(reasoning)+`</div>`, strings.Repeat(" ", 12), "")
		}
		blocks = append(blocks, html+"  </div></div>")
	}
	return strings.Join(blocks, "\n"), nil
}

// Decoded JSON object of a value, nil when it is not one
func asExportObject(v any) *jsonutil.Object {
	switch x := v.(type) {
	case *jsonutil.Object:
		return x
	case map[string]any:
		obj := jsonutil.NewObject()
		for k, value := range x {
			obj.Set(k, value)
		}
		return obj
	}
	return nil
}

// Sidebar link of one session with its clipped title, id prefix and start date
func sidebarItemHTML(s *jsonutil.Object) string {
	sid := anyutil.String(htmlGetOr(s, "id", "N/A"))
	title := "Untitled Session"
	for _, key := range []string{"title", "preview"} {
		if v := rowValue(s, key); anyutil.IsTrue(v) {
			title = anyutil.String(v)
			break
		}
	}
	if utf8.RuneCountInString(title) > 50 {
		title = textutil.First(title, 47) + "..."
	}
	date, _, _ := strings.Cut(htmlFormatTimestamp(htmlGetOr(s, "started_at", 0)), " ")
	return "\n            <a class=\"session-item\" data-id=\"" + escapeHTML(sid) + "\" href=\"#" + pyurl.Quote(sid, "") + "\">\n" +
		"                <div class=\"session-item-title\">" + escapeHTML(title) + "</div>\n" +
		"                <div class=\"session-item-meta\">\n" +
		"                    <span>" + escapeHTML(textutil.First(sid, min(8, utf8.RuneCountInString(sid)))) + "</span>\n" +
		"                    <span>" + date + "</span>\n" +
		"                </div>\n" +
		"            </a>\n" +
		"            "
}

// Sidebar of a multi session export with the brand, the search box and one link per session
func sidebarHTML(sessions []*jsonutil.Object) string {
	var items strings.Builder
	for _, s := range sessions {
		items.WriteString(sidebarItemHTML(s))
	}
	return "\n        <aside class=\"sidebar\">\n" +
		"            <div class=\"sidebar-header\">\n" +
		"                <div class=\"sidebar-brand\">\n" +
		"                    " + iconIcebath + " Icebath History\n" +
		"                </div>\n" +
		"                <div class=\"search-container\">\n" +
		"                    " + iconSearch + "\n" +
		"                    <input type=\"text\" id=\"session-search\" placeholder=\"Search sessions...\">\n" +
		"                </div>\n" +
		"            </div>\n" +
		"            <div class=\"session-list\">\n" +
		"                " + items.String() + "\n" +
		"            </div>\n" +
		"        </aside>\n" +
		"        "
}

// One session view with its header, its system prompt section and its messages
func sessionViewHTML(s *jsonutil.Object, isMulti bool) (string, error) {
	escapedSID := escapeHTML(anyutil.String(htmlGetOr(s, "id", "N/A")))
	systemHTML := ""
	if prompt := rowValue(s, "system_prompt"); anyutil.IsTrue(prompt) {
		systemHTML = htmlCollapsible("system-prompt", iconShield, "System Prompt (Persona)", `<div class="content">`+escapeHTML(prompt)+`</div>`,
			strings.Repeat(" ", 12), "system-prompt-section active")
	}
	viewClass := "session-view active"
	if isMulti {
		viewClass = "session-view"
	}
	title, model := any("Icebath Session"), any("Unknown")
	if v := rowValue(s, "title"); anyutil.IsTrue(v) {
		title = v
	}
	if v := rowValue(s, "model"); anyutil.IsTrue(v) {
		model = v
	}
	messages, err := generateMessagesHTML(exportDisplayRows(s))
	if err != nil {
		return "", err
	}
	return "\n        <div class=\"" + viewClass + "\" id=\"view-" + escapedSID + "\">\n" +
		"            <header class=\"fade-in\">\n" +
		"                <h1>" + escapeHTML(title) + "</h1>\n" +
		"                <div class=\"meta\">\n" +
		"                    <div class=\"meta-item\"><strong>ID:</strong> " + escapedSID + "</div>\n" +
		"                    <div class=\"meta-item\"><strong>Model:</strong> " + escapeHTML(model) + "</div>\n" +
		"                    <div class=\"meta-item\"><strong>Started:</strong> " + htmlFormatTimestamp(htmlGetOr(s, "started_at", 0)) + "</div>\n" +
		"                </div>\n" +
		"                " + systemHTML + "\n" +
		"            </header>\n" +
		"            <main>\n" +
		"                " + messages + "\n" +
		"            </main>\n" +
		"        </div>\n" +
		"        ", nil
}

// Fills the page template for the sessions with a generation time and a script nonce
func renderHTMLExport(sessions []*jsonutil.Object, generatedAt, nonce string) (string, error) {
	isMulti := len(sessions) > 1
	pageTitle, sidebar, margin, layout := "Icebath Session Export", sidebarHTML(sessions), "var(--sidebar-width)", "layout-multi"
	if !isMulti {
		title := any("Icebath Session")
		if v := rowValue(sessions[0], "title"); anyutil.IsTrue(v) {
			title = v
		}
		pageTitle, sidebar, margin, layout = escapeHTML(title), "", "0", "layout-single"
	}
	views := make([]string, 0, len(sessions))
	for _, s := range sessions {
		view, err := sessionViewHTML(s, isMulti)
		if err != nil {
			return "", err
		}
		views = append(views, view)
	}
	return templateutil.Format(htmlExportTemplate, map[string]any{"page_title": pageTitle, "sidebar_html": sidebar,
		"sessions_html": strings.Join(views, "\n"), "main_margin": margin, "layout_class": layout, "generated_at": generatedAt, "script_nonce": nonce})
}

// One self contained HTML page for the sessions, a sidebar switching between several
func GenerateMultiSessionHTMLExport(sessions []*jsonutil.Object) (string, error) {
	if len(sessions) == 0 {
		return "<html><body><h1>No sessions to export.</h1></body></html>", nil
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return "", err
	}
	return renderHTMLExport(sessions, time.Now().Format(time.DateTime), base64.RawURLEncoding.EncodeToString(token))
}

// Self contained HTML page of one session
func GenerateHTMLExport(session *jsonutil.Object) (string, error) {
	return GenerateMultiSessionHTMLExport([]*jsonutil.Object{session})
}
