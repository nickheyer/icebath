package export

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/base64"
	"html/template"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "embed"

	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

//go:embed page.html
var pageSource string

var page = template.Must(template.New("page").Parse(pageSource))

var roleClass = regexp.MustCompile(`[^A-Za-z0-9_-]`)

type htmlCall struct {
	Name, Arguments string
}

type htmlMessage struct {
	Class, Role, Icon, Time, Text, Reasoning string
	Pre                                      bool
	Delay                                    string
	Calls                                    []htmlCall
}

type htmlSession struct {
	ID, Anchor, Title, Link, Short, Date, Model, Started, Prompt string
	Messages                                                     []htmlMessage
}

// Page that shows the sessions, one view each with a sidebar to pick and search them when there are several; works offline, its script
// allowed only by the nonce the page carries
func HTML(transcripts []*sessionsv1.Transcript) ([]byte, error) {
	if len(transcripts) == 0 {
		return []byte("<html><body><h1>No sessions to export.</h1></body></html>"), nil
	}
	nonce := make([]byte, 16)
	rand.Read(nonce)
	data := struct {
		Title, Layout, Generated, Nonce string
		Multi                           bool
		Sessions                        []htmlSession
	}{Title: "Icebath Session Export", Layout: "layout-multi", Generated: time.Now().Format("2006-01-02 15:04:05"),
		Nonce: base64.RawURLEncoding.EncodeToString(nonce), Multi: len(transcripts) > 1}
	if !data.Multi {
		data.Title, data.Layout = cmp.Or(transcripts[0].GetSession().GetTitle(), "Icebath Session"), "layout-single"
	}
	for _, t := range transcripts {
		s := t.GetSession()
		link := cmp.Or(s.GetTitle(), cmp.Or(s.GetPreview(), "Untitled Session"))
		if runes := []rune(link); len(runes) > 50 {
			link = string(runes[:47]) + "..."
		}
		view := htmlSession{ID: s.GetId(), Anchor: url.PathEscape(s.GetId()), Title: cmp.Or(s.GetTitle(), "Icebath Session"), Link: link,
			Short: strs.Head(s.GetId(), 8), Date: "N/A", Model: cmp.Or(s.GetModel(), "Unknown"), Started: "N/A", Prompt: t.GetSystemPrompt()}
		if s.GetCreated() != nil {
			view.Date, view.Started = s.GetCreated().AsTime().Local().Format("2006-01-02"), s.GetCreated().AsTime().Local().Format("2006-01-02 15:04:05")
		}
		for i, m := range shown(t.GetMessages()) {
			role := Role(m)
			icon := map[string]string{"user": "user", "assistant": "bot", "system": "shield"}[role]
			entry := htmlMessage{Class: "message-" + roleClass.ReplaceAllString(strings.ToLower(role), "-"), Role: role, Icon: cmp.Or(icon, "terminal"),
				Time: "N/A", Text: m.GetText(), Reasoning: m.GetReasoning(), Pre: role == "tool"}
			if m.GetTime() != nil {
				entry.Time = m.GetTime().AsTime().Local().Format("2006-01-02 15:04:05")
			}
			if i < 10 {
				entry.Delay = strconv.FormatFloat(float64(i)*0.05, 'f', -1, 64) + "s"
			}
			for _, call := range m.GetToolCalls() {
				entry.Calls = append(entry.Calls, htmlCall{Name: cmp.Or(call.GetName(), "unknown"), Arguments: cmp.Or(call.GetArguments(), "{}")})
			}
			view.Messages = append(view.Messages, entry)
		}
		data.Sessions = append(data.Sessions, view)
	}
	var b bytes.Buffer
	if err := page.Execute(&b, data); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
