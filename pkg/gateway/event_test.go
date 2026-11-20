package gateway

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

var _ config.WakeEvent = (*MessageEvent)(nil)

func TestMessageEventIsCommand(t *testing.T) {
	for text, want := range map[string]bool{
		"/new": true,
		"@image:/tmp/screenshot.png\n\n/moa what is this?":                         true,
		"@image:C:\\Users\\John Doe\\My Pictures\\photo.png\n\n/moa what is this?": true,
		"@file:/tmp/report.pdf\n\n/compress":                                       true,
		"@url:https://example.com\n\n/status":                                      true,
		"@image:C:\\Users\\john@doe\\photo.png\n\n/moa hi":                         true,
		"@image:/tmp/foo.png\n\nwhat is this?":                                     false,
	} {
		if got := NewMessageEvent(text).IsCommand(); got != want {
			t.Fatalf("IsCommand(%q) = %v", text, got)
		}
	}
	ev := NewMessageEvent("/new")
	ev.AllowGatewayControl = false
	if ev.IsCommand() {
		t.Fatal("an event without gateway control is never a command")
	}
}

func TestMessageEventGetCommand(t *testing.T) {
	for text, want := range map[string]string{"/new": "new", "/reset session": "reset", "@image:/tmp/foo.png\n\n/moa something interesting": "moa", "/Help@MyBot x": "help"} {
		if got, ok := NewMessageEvent(text).GetCommand(); !ok || got != want {
			t.Fatalf("GetCommand(%q) = %q %v", text, got, ok)
		}
	}
	for _, text := range []string{"hello", "/usr/bin/ls"} {
		if got, ok := NewMessageEvent(text).GetCommand(); ok {
			t.Fatalf("GetCommand(%q) = %q, want None", text, got)
		}
	}
}

func TestMessageEventGetCommandArgs(t *testing.T) {
	for text, want := range map[string]string{
		"/new session id 123":                    "session id 123",
		"/status check @url:https://example.com": "check @url:https://example.com",
		"/model \u2014\u2014global x \u2013y":    "--global x -y",
		"/new":                                   "",
		"plain words":                            "plain words",
	} {
		if got := NewMessageEvent(text).GetCommandArgs(); got != want {
			t.Fatalf("GetCommandArgs(%q) = %q", text, got)
		}
	}
}

func TestMessageEventMediaAndReplyMerging(t *testing.T) {
	a := NewMessageEvent("a")
	a.MediaURLs, a.MediaTypes = []string{"/c/1"}, []string{"image/png"}
	b := NewMessageEvent("b")
	b.MediaURLs, b.MediaTypes, b.MediaFilenames = []string{"/c/2", "/c/3"}, []string{"image/png", "text/plain"}, []string{"two.png"}
	a.ExtendMedia(b)
	if strings.Join(a.MediaFilenames, ",") != ",two.png," || len(a.MediaURLs) != 3 || len(a.MediaTypes) != 3 {
		t.Fatalf("merged media %#v %#v", a.MediaFilenames, a.MediaURLs)
	}
	c, d := NewMessageEvent("c"), NewMessageEvent("d")
	c.ExtendMedia(d)
	if c.MediaFilenames != nil {
		t.Fatal("unnamed events keep their names untouched")
	}
	yes, no := true, false
	c.ReplyExpected = &no
	d.ReplyExpected = nil
	c.AbsorbReplyExpected(d)
	if c.ReplyExpected != nil {
		t.Fatal("an unknown expectation replaces a false one")
	}
	c.ReplyExpected = &yes
	d.ReplyExpected = &no
	c.AbsorbReplyExpected(d)
	if !*c.ReplyExpected {
		t.Fatal("an addressed message wins")
	}
}

func TestCleanAttachmentFilename(t *testing.T) {
	long := strings.Repeat("x", 300) + ".pdf"
	for in, want := range map[any]string{
		"C:\\Users\\me\\report.pdf": "report.pdf",
		"../../etc/passwd":          "passwd",
		"bad\r\nname\u202e.txt":     "badname.txt",
		"  ..  ":                    "",
		"dir/":                      "",
		42:                          "",
		long:                        strings.Repeat("x", 251) + ".pdf",
	} {
		if got := CleanAttachmentFilename(in); got != want {
			t.Fatalf("CleanAttachmentFilename(%q) = %q", in, got)
		}
	}
	if got := CleanAttachmentFilename(strings.Repeat("y", 260) + "." + strings.Repeat("e", 17)); len(got) != 255 || strings.Contains(got, ".") {
		t.Fatalf("an over long extension is cut with the name, got %d chars", len(got))
	}
}
