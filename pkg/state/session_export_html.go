package state

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
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
	iconIcebath      = "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"24\" height=\"24\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"#FFD700\" stroke-width=\"2\" stroke-linecap=\"round\" stroke-linejoin=\"round\"><path d=\"M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z\"/><path d=\"m9 12 2 2 4-4\"/></svg>"
)

//go:embed session_export_template.html
var htmlExportTemplate string

var (
	htmlRoleIcons = map[string]string{"user": iconUser, "assistant": iconBot, "system": iconShield}
	htmlChevron   = strings.ReplaceAll(iconChevronRight, `class="`, `class="chevron `)
)

// Text of a value with the five HTML special characters escaped
func escapeHTML(text any) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(pyrepr.Str(text))
}

// Local date and time of a stored timestamp, N/A when unset or corrupt
func htmlFormatTimestamp(ts any) string {
	if !pyrepr.Truthy(ts) {
		return "N/A"
	}
	epoch, ok := pydatetime.CoerceEpoch(ts, "", "timestamp")
	if !ok || epoch == 0 {
		return "N/A"
	}
	return pydatetime.Strftime(pydatetime.FromTimestamp(epoch), "%Y-%m-%d %H:%M:%S")
}

// Display text of a message content, the text parts of a multimodal list joined and images named
func htmlContentText(content any) (any, error) {
	list, ok := exportJSONValue(content).([]any)
	if !ok {
		return exportJSONValue(content), nil
	}
	var parts []string
	for _, part := range list {
		obj, isObject := part.(*pyjson.Object)
		if !isObject {
			if m, isMap := part.(map[string]any); isMap {
				obj, isObject = pyjson.NewObject(), true
				for k, v := range m {
					obj.Set(k, v)
				}
			}
		}
		switch {
		case !isObject:
			parts = append(parts, pyrepr.Str(part))
		case rowValue(obj, "type") == "text":
			text, present := obj.Get("text")
			if !present {
				text = ""
			}
			s, isString := text.(string)
			if !isString {
				return nil, &pyint.Error{Kind: "TypeError", Msg: "sequence item " + pyrepr.Str(len(parts)) + ": expected str instance, " + pyrepr.TypeName(text) + " found"}
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
func htmlGetOr(row *pyjson.Object, key string, fallback any) any {
	if row != nil {
		if v, ok := row.Get(key); ok {
			return v
		}
	}
	return fallback
}

// Message blocks of a session, tool calls and reasoning folded, every stored text escaped
func generateMessagesHTML(messages []*pyjson.Object) (string, error) {
	var blocks []string
	for i, msg := range messages {
		role := htmlGetOr(msg, "role", "unknown")
		if role == "session_meta" {
			continue
		}
		raw := rowValue(msg, "content")
		if !pyrepr.Truthy(raw) {
			raw = ""
		}
		content, err := htmlContentText(raw)
		if err != nil {
			return "", err
		}
		var roleClass strings.Builder
		for _, c := range pystr.Lower(pyrepr.Str(role)) {
			if pystr.IsAlnum(c) || c == '-' || c == '_' {
				roleClass.WriteRune(c)
			} else {
				roleClass.WriteByte('-')
			}
		}
		delayStyle := ""
		if i < 10 {
			delayStyle = ` style="animation-delay: ` + pyrepr.Float(min(float64(i)*0.05, 1.0)) + `s"`
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
		if calls := exportJSONValue(rowValue(msg, "tool_calls")); pyrepr.Truthy(calls) {
			list, _ := calls.([]any)
			for _, tc := range list {
				fn, _ := htmlGetOr(asExportObject(tc), "function", pyjson.NewObject()).(*pyjson.Object)
				html += htmlCollapsible("tool-call", iconWrench, "Tool Call: "+escapeHTML(htmlGetOr(fn, "name", "unknown")),
					"<pre><code>"+escapeHTML(htmlGetOr(fn, "arguments", "{}"))+"</code></pre>", strings.Repeat(" ", 16), "")
			}
		}
		if pyrepr.Truthy(content) {
			escaped := escapeHTML(content)
			if role == "tool" {
				escaped = "<pre><code>" + escaped + "</code></pre>"
			}
			html += `  <div class="content">` + escaped + `</div>`
		}
		reasoning := rowValue(msg, "reasoning")
		if !pyrepr.Truthy(reasoning) {
			reasoning = rowValue(msg, "reasoning_content")
		}
		if pyrepr.Truthy(reasoning) {
			html += htmlCollapsible("reasoning", iconSparkles, "Reasoning", `<div class="content">`+escapeHTML(reasoning)+`</div>`, strings.Repeat(" ", 12), "")
		}
		blocks = append(blocks, html+"  </div></div>")
	}
	return strings.Join(blocks, "\n"), nil
}

// Decoded JSON object of a value, nil when it is not one
func asExportObject(v any) *pyjson.Object {
	switch x := v.(type) {
	case *pyjson.Object:
		return x
	case map[string]any:
		obj := pyjson.NewObject()
		for k, value := range x {
			obj.Set(k, value)
		}
		return obj
	}
	return nil
}

// Sidebar link of one session with its clipped title, id prefix and start date
func sidebarItemHTML(s *pyjson.Object) string {
	sid := pyrepr.Str(htmlGetOr(s, "id", "N/A"))
	title := "Untitled Session"
	for _, key := range []string{"title", "preview"} {
		if v := rowValue(s, key); pyrepr.Truthy(v) {
			title = pyrepr.Str(v)
			break
		}
	}
	if utf8.RuneCountInString(title) > 50 {
		title = pystr.Slice(title, 0, 47) + "..."
	}
	date, _, _ := strings.Cut(htmlFormatTimestamp(htmlGetOr(s, "started_at", 0)), " ")
	return "\n            <a class=\"session-item\" data-id=\"" + escapeHTML(sid) + "\" href=\"#" + pyurl.Quote(sid, "") + "\">\n" +
		"                <div class=\"session-item-title\">" + escapeHTML(title) + "</div>\n" +
		"                <div class=\"session-item-meta\">\n" +
		"                    <span>" + escapeHTML(pystr.Slice(sid, 0, min(8, utf8.RuneCountInString(sid)))) + "</span>\n" +
		"                    <span>" + date + "</span>\n" +
		"                </div>\n" +
		"            </a>\n" +
		"            "
}

// Sidebar of a multi session export with the brand, the search box and one link per session
func sidebarHTML(sessions []*pyjson.Object) string {
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
func sessionViewHTML(s *pyjson.Object, isMulti bool) (string, error) {
	escapedSID := escapeHTML(pyrepr.Str(htmlGetOr(s, "id", "N/A")))
	systemHTML := ""
	if prompt := rowValue(s, "system_prompt"); pyrepr.Truthy(prompt) {
		systemHTML = htmlCollapsible("system-prompt", iconShield, "System Prompt (Persona)", `<div class="content">`+escapeHTML(prompt)+`</div>`,
			strings.Repeat(" ", 12), "system-prompt-section active")
	}
	viewClass := "session-view active"
	if isMulti {
		viewClass = "session-view"
	}
	title, model := any("Icebath Session"), any("Unknown")
	if v := rowValue(s, "title"); pyrepr.Truthy(v) {
		title = v
	}
	if v := rowValue(s, "model"); pyrepr.Truthy(v) {
		model = v
	}
	messages, err := generateMessagesHTML(exportMessageRows(s))
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
func renderHTMLExport(sessions []*pyjson.Object, generatedAt, nonce string) (string, error) {
	isMulti := len(sessions) > 1
	pageTitle, sidebar, margin, layout := "Icebath Session Export", sidebarHTML(sessions), "var(--sidebar-width)", "layout-multi"
	if !isMulti {
		title := any("Icebath Session")
		if v := rowValue(sessions[0], "title"); pyrepr.Truthy(v) {
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
	return pyrepr.FormatMap(htmlExportTemplate, map[string]any{"page_title": pageTitle, "sidebar_html": sidebar,
		"sessions_html": strings.Join(views, "\n"), "main_margin": margin, "layout_class": layout, "generated_at": generatedAt, "script_nonce": nonce})
}

// One self contained HTML page for the sessions, a sidebar switching between several
func GenerateMultiSessionHTMLExport(sessions []*pyjson.Object) (string, error) {
	if len(sessions) == 0 {
		return "<html><body><h1>No sessions to export.</h1></body></html>", nil
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return "", err
	}
	return renderHTMLExport(sessions, pydatetime.Strftime(time.Now(), "%Y-%m-%d %H:%M:%S"), base64.RawURLEncoding.EncodeToString(token))
}

// Self contained HTML page of one session
func GenerateHTMLExport(session *pyjson.Object) (string, error) {
	return GenerateMultiSessionHTMLExport([]*pyjson.Object{session})
}
