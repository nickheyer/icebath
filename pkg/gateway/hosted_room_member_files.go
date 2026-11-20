package gateway

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Kind and MIME type the home gateway files bytes under, told by their leading bytes and then by their name
func roomFileType(name string, data []byte) (string, string) {
	sniffed := sniffKnownMime(data)
	switch {
	case strings.HasPrefix(sniffed, "image/"):
		return "image", sniffed
	case sniffed == "application/pdf":
		return "pdf", sniffed
	}
	declared, _, _ := strings.Cut(mime.TypeByExtension(strings.ToLower(filepath.Ext(name))), ";")
	declared = strings.ToLower(strings.TrimSpace(declared))
	switch {
	case declared == "" || strings.HasPrefix(declared, "image/") || declared == "application/pdf" || !attachmentMimeRe.MatchString(declared):
		declared = sniffed
	case strings.HasPrefix(declared, "text/") && sniffed != "text/plain":
		declared = "application/octet-stream"
	}
	return "file", declared
}

// Size in binary units for the texts that name a room file limit
func roomBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1024), ".0") + " KiB"
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/(1<<20)), ".0") + " MiB"
}

// Bounded basename a room stores a file under, unsafe characters replaced
func roomFileName(path string) string {
	name := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 || r == '\n' || r == '\r' {
			return '_'
		}
		return r
	}, strings.TrimSpace(filepath.Base(path)))
	for utf8.RuneCountInString(name) > maxAttachmentNameChars {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}

// Manifest item of bytes stored as one room upload under the type the home gateway gives them
func storeRoomFile(store *HostedRoomAttachmentStore, roomID, uploadID, name string, data []byte) (map[string]any, error) {
	kind, mimeType := roomFileType(name, data)
	saved, err := store.Put(roomID, uploadID, kind, roomFileName(name), mimeType, data)
	if err != nil {
		return nil, err
	}
	return map[string]any{"attachment_id": saved["attachment_id"], "kind": saved["kind"], "name": saved["name"], "mime": saved["mime"],
		"size": saved["size"]}, nil
}

// Appends one publication event, a member message's files committed to every member first and released when the append fails
func (s *HostedRoomService) appendMemberMessage(room map[string]any, in AppendEventInput) (map[string]any, error) {
	payload, _ := hostedMapping(in.Payload)
	manifest, has := payload["attachments"]
	if in.Kind != "message.member" || !has {
		return AppendEvent(s.ctx, s.DBPath, in)
	}
	store, err := NewHostedRoomAttachmentStore(s.ctx, s.DBPath, AttachmentStoreOptions{})
	if err != nil {
		return nil, err
	}
	recipients := []string{}
	members, _ := room["members"].([]any)
	for _, raw := range members {
		member, _ := hostedMapping(raw)
		recipients = append(recipients, pyrepr.Str(member["member_id"]))
	}
	_, transitioned, err := store.CommitMessageWithReceipt(AttachmentCommit{RoomID: in.RoomID, EventID: in.EventID, Manifest: manifest,
		RecipientMemberIDs: recipients, ViewerAccess: true, HoldUntilEvent: true})
	if err != nil {
		return nil, err
	}
	event, err := AppendEvent(s.ctx, s.DBPath, in)
	if err != nil && len(transitioned) > 0 {
		if _, abortErr := store.AbortMessageCommit(in.RoomID, in.EventID, transitioned); abortErr != nil {
			return nil, abortErr
		}
	}
	return event, err
}

// Reply text without MEDIA directives and the room files they name, each file left out named with its reason
func memberReplyFiles(ctx context.Context, dbPath, roomID, uploadPrefix, text string) (string, []any, error) {
	if !strings.Contains(text, "MEDIA:") {
		return text, nil, nil
	}
	media, cleaned, err := ExtractMedia(ctx, text)
	if err != nil || len(media) == 0 {
		return text, nil, err
	}
	var dropped []map[string]any
	safe, err := FilterMediaDeliveryPathsFor(ctx, media, "", &dropped)
	if err != nil {
		return "", nil, err
	}
	var notes []string
	for _, item := range dropped {
		notes = append(notes, fmt.Sprintf("[%s was not shared: %v]", roomFileName(fmt.Sprint(item["path"])), item["reason"]))
	}
	store, err := NewHostedRoomAttachmentStore(ctx, dbPath, AttachmentStoreOptions{})
	if err != nil {
		return "", nil, err
	}
	manifest := []any{}
	var total int64
	for index, ref := range safe {
		name := roomFileName(ref.Path)
		info, err := os.Stat(ref.Path)
		if err != nil {
			notes = append(notes, fmt.Sprintf("[%s was not shared: %s]", name, err.Error()))
			continue
		}
		size := info.Size()
		switch {
		case len(manifest) >= maxAttachmentsPerMessage:
			notes = append(notes, fmt.Sprintf("[%s was not shared: one message brings at most %d files]", name, maxAttachmentsPerMessage))
			continue
		case size > maxAttachmentBytes:
			notes = append(notes, fmt.Sprintf("[%s was not shared: %s is over the %s limit for one room file]", name, roomBytes(size), roomBytes(maxAttachmentBytes)))
			continue
		case total+size > maxMessageAttachmentBytes:
			notes = append(notes, fmt.Sprintf("[%s was not shared: the files of one message stay under %s]", name, roomBytes(maxMessageAttachmentBytes)))
			continue
		case size == 0:
			notes = append(notes, fmt.Sprintf("[%s was not shared: the file is empty]", name))
			continue
		}
		data, err := os.ReadFile(ref.Path)
		if err != nil {
			notes = append(notes, fmt.Sprintf("[%s was not shared: %s]", name, err.Error()))
			continue
		}
		entry, err := storeRoomFile(store, roomID, fmt.Sprintf("%s.%d", uploadPrefix, index), name, data)
		var refused *AttachmentError
		if errors.As(err, &refused) {
			notes = append(notes, fmt.Sprintf("[%s was not shared: %s]", name, refused.Msg))
			continue
		}
		if err != nil {
			return "", nil, err
		}
		manifest = append(manifest, entry)
		total += int64(len(data))
	}
	if len(notes) > 0 {
		cleaned = strings.TrimSpace(cleaned + "\n\n" + strings.Join(notes, "\n"))
	}
	return cleaned, manifest, nil
}
