package app

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/sessions/export"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Folder a session saved with no file named goes to, under the folder of icebath's own files
var savedFolder = filepath.Join("sessions", "saved")

// What /save says when run bare or with a format it doesn't know: what it does, how it is typed, the formats, where the file goes and examples
func saveUsage() string {
	folder := "the saved sessions folder"
	if path, err := export.Folder(savedFolder); err == nil {
		folder = profile.Tilde(path)
	}
	return "/save writes this session to a file.\n" +
		"Usage: /save " + saveUsageLine + "\n\n" +
		"Formats\n" +
		"  md       the messages as readable Markdown\n" +
		"  json     every message with all its details, for other programs\n" +
		"  html     one page to open in a browser, with nothing else needed\n\n" +
		"Options\n" +
		"  file     where to write it. A name alone goes in the folder of the session.\n" +
		"           With no file, icebath names it and puts it in " + folder + ".\n" +
		"  redact   hide API keys and other secrets in the file\n\n" +
		"Examples\n" +
		"  /save md\n" +
		"  /save html\n" +
		"  /save md notes.md\n" +
		"  /save html session.html redact"
}

// Format a name given for one stands for, empty for none
func saveFormat(name string) string {
	switch strings.ToLower(name) {
	case "json", "snapshot":
		return "json"
	case "md", "markdown":
		return "md"
	case "html":
		return "html"
	}
	return ""
}

// Writes the session to a file in the format: the file named, under the home folder for ~, under the session's working directory when
// relative, else the default name in the saved sessions folder; secrets masked when asked. The path written
func (d *desk) save(ctx context.Context, id, format, file string, redact bool) (string, error) {
	response, err := d.sessions.Export(ctx, &sessionsv1.ExportRequest{Filter: &sessionsv1.Filter{Ids: []string{id}, IncludeArchived: true,
		IncludeHidden: true, IncludeChildren: true, IncludeEmpty: true, IncludeOpen: true, IncludePinned: true}, Capped: true})
	if err != nil {
		return "", errors.New(tui.Reason(err))
	}
	transcripts := response.GetTranscripts()
	if len(transcripts) == 0 || len(transcripts[0].GetMessages()) == 0 {
		return "", errors.New("This session has no stored messages yet.")
	}
	if redact {
		transcripts = export.Redact(transcripts)
	}
	var data []byte
	switch format {
	case "json":
		data, err = export.JSON(transcripts[0])
	case "md":
		data = export.Markdown(transcripts)
	case "html":
		data, err = export.HTML(transcripts)
	}
	if err != nil {
		return "", err
	}
	path, err := savePath(transcripts[0].GetSession(), format, file)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func savePath(session *sessionsv1.Session, format, file string) (string, error) {
	file = strings.TrimSpace(file)
	if file == "" {
		folder, err := export.Folder(savedFolder)
		if err != nil {
			return "", err
		}
		return filepath.Join(folder, export.DefaultName(session.GetId(), format)), nil
	}
	if file == "~" || strings.HasPrefix(file, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(file, "~")), nil
	}
	if filepath.IsAbs(file) {
		return file, nil
	}
	base := session.GetDirectory()
	if base == "" {
		var err error
		if base, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	return filepath.Join(base, file), nil
}

// /save <json|md|html> [file] [redact]
func (c *chat) saveTo(argument string) (tea.Cmd, bool) {
	echo := strings.TrimSpace("/save " + argument)
	words := strings.Fields(argument)
	redact := false
	if n := len(words); n > 0 {
		switch strings.ToLower(words[n-1]) {
		case "redact", "-redact", "--redact":
			redact, words = true, words[:n-1]
		}
	}
	if len(words) == 0 {
		c.say(echo, "```\n"+saveUsage()+"\n```")
		return nil, true
	}
	format := saveFormat(words[0])
	if format == "" {
		c.say(echo, "There is no format “"+words[0]+"”. Use md, json or html.\n\n```\n"+saveUsage()+"\n```")
		return nil, true
	}
	id := c.id
	if id == "" {
		c.say(echo, "This chat has no session yet, so there is nothing to save.")
		return nil, true
	}
	if !c.log.conversed() {
		c.say(echo, "This session has no messages yet, so there is nothing to save.")
		return nil, true
	}
	file := strings.Join(words[1:], " ")
	return tui.Async(func() func() tea.Cmd {
		path, err := c.desk.save(c.kit.Env.Ctx, id, format, file, redact)
		return func() tea.Cmd {
			if err != nil {
				c.say(echo, "The session was not saved. "+err.Error())
				return nil
			}
			c.say(echo, "Saved the session to `"+path+"`.")
			return nil
		}
	}), true
}

// /branch [--here] [title]: copies the session into a new one that goes its own way, which this window then shows
func (c *chat) branch(argument string) (tea.Cmd, bool) {
	echo := strings.TrimSpace("/branch " + argument)
	words := strings.Fields(argument)
	kept := words[:0]
	for _, word := range words {
		if word != "--here" {
			kept = append(kept, word)
		}
	}
	title := strings.Join(kept, " ")
	id := c.id
	if id == "" {
		c.say(echo, "This chat has no session yet, so there is nothing to branch.")
		return nil, true
	}
	if !c.log.conversed() {
		c.say(echo, "This session has no messages yet. Send one first, then branch it.")
		return nil, true
	}
	return tui.Async(func() func() tea.Cmd {
		response, err := c.desk.sessions.Branch(c.kit.Env.Ctx, &sessionsv1.BranchRequest{Id: id, Title: title})
		return func() tea.Cmd {
			if err != nil {
				c.say(echo, "The branch was not made. "+tui.Reason(err))
				return nil
			}
			child := response.GetSession()
			cmd := c.switchTo(child)
			c.say(echo, "This chat now shows the branch "+label(child.GetId(), cmp.Or(child.GetTitle(), title))+". The session it came from stays in /sessions.")
			return cmd
		}
	}), true
}
