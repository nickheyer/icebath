package discord

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// DM message carrying attachments served by the fake CDN
func documentMessage(f *fakeDiscordAPI, content string, files map[string]string, atts ...*discordgo.MessageAttachment) *inboundMessage {
	if f.get == nil {
		f.get = map[string]string{}
	}
	for path, body := range files {
		f.get[path] = body
	}
	m := makeMessage(dmChannel("dm1"), content)
	m.Author = &discordgo.User{ID: "42", Username: "Tester"}
	m.Attachments = atts
	return m
}

func attachment(name, contentType string, size int) *discordgo.MessageAttachment {
	return &discordgo.MessageAttachment{ID: name, Filename: name, ContentType: contentType, Size: size, URL: "https://cdn.discordapp.com/attachments/fake/" + name}
}

func lastEvent(t *testing.T, c *eventCapture) *gateway.MessageEvent {
	t.Helper()
	all := c.all()
	if len(all) == 0 {
		t.Fatal("handle_message not called")
	}
	return all[len(all)-1]
}

func TestIncomingDocumentHandlingTxtContentInjected(t *testing.T) {
	a, f, c := inboundAdapter(t)
	m := documentMessage(f, "summarize this", map[string]string{"/attachments/fake/notes.txt": "Hello from a text file"}, attachment("notes.txt", "text/plain", 1024))
	a.handleDiscordMessage(t.Context(), m, false, false)
	ev := lastEvent(t, c)
	if !strings.Contains(ev.Text, "[Content of notes.txt]:") || !strings.Contains(ev.Text, "Hello from a text file") || !strings.Contains(ev.Text, "summarize this") {
		t.Fatalf("text %q", ev.Text)
	}
	if strings.Index(ev.Text, "[Content of") > strings.Index(ev.Text, "summarize this") {
		t.Fatal("injection must precede the user text")
	}
}

func TestIncomingDocumentHandlingLargeTxtCachedNotInjected(t *testing.T) {
	a, f, c := inboundAdapter(t)
	large := strings.Repeat("x", 200*1024)
	m := documentMessage(f, "", map[string]string{"/attachments/fake/big.txt": large}, attachment("big.txt", "text/plain", len(large)))
	a.handleDiscordMessage(t.Context(), m, false, false)
	ev := lastEvent(t, c)
	if len(ev.MediaURLs) != 1 {
		t.Fatalf("media %v", ev.MediaURLs)
	}
	if _, err := os.Stat(ev.MediaURLs[0]); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ev.Text, "[Content of") {
		t.Fatal("large file injected")
	}
}

func TestIncomingDocumentHandlingDocumentMarksMediaTextInlined(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		inlined       bool
	}{{"small", "small text", true}, {"large", strings.Repeat("x", 200*1024), false}} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, c := inboundAdapter(t)
			m := documentMessage(f, "", map[string]string{"/attachments/fake/notes.txt": tc.content}, attachment("notes.txt", "text/plain", len(tc.content)))
			a.handleDiscordMessage(t.Context(), m, false, false)
			ev := lastEvent(t, c)
			if strings.Contains(ev.Text, "[Content of") != tc.inlined {
				t.Fatalf("text %q", ev.Text[:min(40, len(ev.Text))])
			}
			if len(ev.MediaTextInlined) != 1 || *ev.MediaTextInlined[0] != tc.inlined {
				t.Fatalf("inlined %v", ev.MediaTextInlined)
			}
		})
	}
}

func TestIncomingDocumentHandlingMultipleTextFilesBothInjected(t *testing.T) {
	a, f, c := inboundAdapter(t)
	m := documentMessage(f, "", map[string]string{"/attachments/fake/file1.txt": "First file content", "/attachments/fake/file2.txt": "Second file content"},
		attachment("file1.txt", "text/plain", 1024), attachment("file2.txt", "text/plain", 1024))
	a.handleDiscordMessage(t.Context(), m, false, false)
	ev := lastEvent(t, c)
	for _, want := range []string{"[Content of file1.txt]:", "First file content", "[Content of file2.txt]:", "Second file content"} {
		if !strings.Contains(ev.Text, want) {
			t.Fatalf("missing %q in %q", want, ev.Text)
		}
	}
	if strings.Index(ev.Text, "file1") > strings.Index(ev.Text, "file2") {
		t.Fatal("files out of order")
	}
}

func TestAllowAnyAttachmentUnknownTypeCachedByDefault(t *testing.T) {
	a, f, c := inboundAdapter(t)
	m := documentMessage(f, "", map[string]string{"/attachments/fake/weird.xyz": "\x00\x01\x02 binary payload"}, attachment("weird.xyz", "application/x-custom", 1024))
	a.handleDiscordMessage(t.Context(), m, false, false)
	ev := lastEvent(t, c)
	if len(ev.MediaURLs) != 1 || !slices.Equal(ev.MediaTypes, []string{"application/x-custom"}) || ev.MessageType != gateway.MessageTypeDocument {
		t.Fatalf("urls %v types %v type %v", ev.MediaURLs, ev.MediaTypes, ev.MessageType)
	}
	if _, err := os.Stat(ev.MediaURLs[0]); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ev.Text, "[Content of") {
		t.Fatal("binary injected")
	}
}

func TestAllowAnyAttachmentMaxAttachmentBytesCapsUploads(t *testing.T) {
	a, f, c := inboundAdapter(t)
	a.Config.Extra["max_attachment_bytes"] = 1024
	m := documentMessage(f, "", nil, attachment("too_big.xyz", "application/x-custom", 2048))
	a.handleDiscordMessage(t.Context(), m, false, false)
	if ev := lastEvent(t, c); len(ev.MediaURLs) != 0 {
		t.Fatalf("media %v", ev.MediaURLs)
	}
}

func TestAllowAnyAttachmentMaxAttachmentBytesZeroMeansUnlimited(t *testing.T) {
	a, f, c := inboundAdapter(t)
	a.Config.Extra["max_attachment_bytes"] = 0
	m := documentMessage(f, "", map[string]string{"/attachments/fake/huge.xyz": strings.Repeat("x", 16)}, attachment("huge.xyz", "application/x-custom", 64*1024*1024))
	a.handleDiscordMessage(t.Context(), m, false, false)
	if ev := lastEvent(t, c); len(ev.MediaURLs) != 1 {
		t.Fatalf("media %v", ev.MediaURLs)
	}
}
