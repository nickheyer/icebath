package tuiserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/filedrop"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/stageduploads"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const (
	attachmentMaxBytes     = 256 * 1024 * 1024
	attachBytesMaxBytes    = 25 * 1024 * 1024
	pdfAttachMaxBytes      = 50 * 1024 * 1024
	pdfAttachMaxPages      = 25
	attachmentCopyChunkLen = 1024 * 1024
)

var imageMagic = []struct {
	sig []byte
	ext string
}{{[]byte("\x89PNG\r\n\x1a\n"), ".png"}, {[]byte("\xff\xd8\xff"), ".jpg"}, {[]byte("GIF87a"), ".gif"}, {[]byte("GIF89a"), ".gif"}, {[]byte("BM"), ".bmp"}}

var attachmentRefNeedsQuoting = regexp.MustCompile("[\\s()\\[\\]{}<>\"'`]")

var anyMediaDataURL = regexp.MustCompile(`(?is)^data:[^;,]*(?:;[^;,=]+=[^;,]+)*;base64,(.*)$`)

// Refusal of a payload over its cap in whole megabytes
type payloadTooLarge struct{ msg string }

func (e *payloadTooLarge) Error() string      { return e.msg }
func (e *payloadTooLarge) PyTypeName() string { return "OverflowError" }

// Refusal text of a payload over its cap
func tooLarge(label string, size, maxBytes int) string {
	return fmt.Sprintf("%s too large (%d bytes; cap is %d MB)", label, size, maxBytes/(1024*1024))
}

// Text with every unicode whitespace character removed
func stripAllSpace(s string) string {
	var b strings.Builder
	for _, r := range s {
		if !pystr.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Strictly decoded bytes of an optionally data URL wrapped base64 payload, the cap refusal before decoding when the text is already too long
func b64Payload(raw string, dataURL *regexp.Regexp, maxBytes int, label string) ([]byte, error) {
	cleaned := pystr.Strip(raw)
	if m := dataURL.FindStringSubmatch(cleaned); m != nil {
		cleaned = m[1]
	}
	cleaned = stripAllSpace(cleaned)
	if len(cleaned) > 4*((maxBytes+2)/3) {
		padding := len(cleaned) - len(strings.TrimRight(cleaned, "="))
		return nil, &payloadTooLarge{tooLarge(label, len(cleaned)*3/4-padding, maxBytes)}
	}
	payload, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return nil, err
	}
	if len(payload) > maxBytes {
		return nil, &payloadTooLarge{tooLarge(label, len(payload), maxBytes)}
	}
	return payload, nil
}

// Decoded payload of a data URL of the mime prefix or bare base64, nil when invalid and the cap refusal when too large
func decodeAttachBase64(raw, mimePrefix string, maxBytes int, label string) ([]byte, error) {
	pattern := regexp.MustCompile(`(?s)^data:` + regexp.QuoteMeta(mimePrefix) + `[a-zA-Z0-9.+-]*;base64,(.*)$`)
	payload, err := b64Payload(raw, pattern, maxBytes, label)
	var over *payloadTooLarge
	if errors.As(err, &over) {
		return nil, err
	}
	if err != nil {
		return nil, nil
	}
	return payload, nil
}

// Decoded attachment bytes or the 4017 or 4018 error response
func decodeAttachPayload(rid any, rawB64, mimePrefix string, maxBytes int, label, emptyMsg string) ([]byte, map[string]any) {
	data, err := decodeAttachBase64(rawB64, mimePrefix, maxBytes, label)
	if err != nil {
		return nil, rpcErr(rid, 4018, err.Error(), nil)
	}
	if data == nil {
		return nil, rpcErr(rid, 4017, "data is not valid base64", nil)
	}
	if len(data) == 0 {
		return nil, rpcErr(rid, 4017, emptyMsg, nil)
	}
	return data, nil
}

// Image extension from the filename hint, else the magic bytes, else png
func sniffImageExt(imgBytes []byte, filename string) string {
	if filename != "" {
		if suffix := strings.ToLower(pypath.Suffix(filename)); suffix != "" {
			return suffix
		}
	}
	head := imgBytes[:min(16, len(imgBytes))]
	if bytes.HasPrefix(head, []byte("RIFF")) && len(head) >= 12 && string(head[8:12]) == "WEBP" {
		return ".webp"
	}
	for _, m := range imageMagic {
		if bytes.HasPrefix(head, m.sig) {
			return m.ext
		}
	}
	return ".png"
}

// Suffixes accepted as image attachments
func allowedImageExtensions() map[string]bool { return filedrop.ImageExtensions }

// Directory under the session's own home, attachments following the workspace when the profile opts in
func sessionHomeDir(ctx context.Context, session *tuigateway.Session, name string) string {
	profileHome := strOr(session.Get("profile_home"))
	if name == "attachments" && profileAttachmentsStorage(ctx, profileHome) == "workspace" {
		if workspace := sessionAttachmentsWorkspace(ctx, session); workspace != "" {
			return filepath.Join(workspace, ".icebath", "attachments")
		}
	}
	if profileHome == "" {
		profileHome = launchHome()
	}
	return filepath.Join(profileHome, name)
}

// attachments.storage of the session profile's own config, empty unless it opts into workspace
func profileAttachmentsStorage(ctx context.Context, profileHome string) string {
	h := profileHome
	if h == "" {
		h = launchHome()
	}
	cfgPath := filepath.Join(h, "config.yaml")
	exists, err := config.ConfigExists(ctx, cfgPath)
	if err == nil && exists {
		var cfg map[string]any
		cfg, err = config.LoadUserEffective(cfgPath, config.EnvRefLookupContext(ctx))
		if err == nil {
			if attachments, ok := cfg["attachments"].(map[string]any); ok {
				storage := ""
				if truthy(attachments["storage"]) {
					storage = strOr(attachments["storage"])
				}
				return strings.ToLower(pystr.Strip(storage))
			}
			return ""
		}
	}
	var unavailable *config.ConfigBackendUnavailable
	if errors.As(err, &unavailable) {
		logger().Warn(fmt.Sprintf("attachments.storage for %s unavailable (%s); staging in the profile home", h, unavailable.Message))
	}
	return ""
}

// Session workspace when workspace storage can write to it, empty for a remote or missing cwd
func sessionAttachmentsWorkspace(ctx context.Context, session *tuigateway.Session) (workspace string) {
	if cwdIsRemote(ctx, strOr(session.Get("profile_home"))) {
		return ""
	}
	_ = catchPanic(func() {
		resolved, err := pypath.Resolve(sessionCwd(ctx, session))
		if err == nil && isDir(resolved) {
			workspace = resolved
		}
	})
	return workspace
}

// Images directory of the session home
func sessionImagesDir(ctx context.Context, session *tuigateway.Session) string {
	return sessionHomeDir(ctx, session, "images")
}

// Writes image bytes into a private collision free file of the directory and returns its path
func stageImageBytes(directory string, imgBytes []byte, ext, prefix string) (string, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(directory, prefix+"_*"+ext)
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err = f.Write(imgBytes); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// Stages image bytes in the session images dir and queues them for the next submit
func queueAttachedImage(ctx context.Context, session *tuigateway.Session, imgBytes []byte, ext, prefix string) (string, error) {
	session.Set("image_counter", intOr(session.Get("image_counter"))+1)
	path, err := stageImageBytes(sessionImagesDir(ctx, session), imgBytes, ext, prefix)
	if err != nil {
		session.Set("image_counter", max(0, intOr(session.Get("image_counter"))-1))
		return "", err
	}
	images := stringList(session.Get("attached_images"))
	session.Set("attached_images", append(images, path))
	return path, nil
}

// Context reference value quoted when it holds whitespace, brackets or quotes
func formatRefValue(value string) string {
	if value == "" || !attachmentRefNeedsQuoting.MatchString(value) {
		return value
	}
	for _, q := range []string{"`", `"`, "'"} {
		if !strings.Contains(value, q) {
			return q + value + q
		}
	}
	return value
}

// Workspace relative path of an attachment, absolute when outside the workspace
func attachmentRefPath(ctx context.Context, session *tuigateway.Session, target string) string {
	workspace := resolveLax(sessionCwd(ctx, session))
	resolved := resolveLax(target)
	if rel, ok := relativeTo(resolved, workspace); ok {
		return filepath.ToSlash(rel)
	}
	return resolved
}

// Lexical path of target below base like PurePath.relative_to, the base itself giving a dot
func relativeTo(target, base string) (string, bool) {
	rel, err := filepath.Rel(base, target)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

var controlRun = regexp.MustCompile("[\x00-\x1f]+")

// File name of an attachment with control characters replaced and dots trimmed
func sanitizeAttachmentName(name string) string {
	candidate := controlRun.ReplaceAllString(pypath.Name(pystr.Strip(name)), "_")
	candidate = strings.Trim(pystr.Strip(candidate), ".")
	if candidate == "" {
		return "attachment"
	}
	return candidate
}

// Reads at most the cap from a file, refusing one that grew past it after inspection
func readAttachmentBytes(path string, maxBytes int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	payload, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxBytes {
		info, statErr := f.Stat()
		if statErr != nil {
			return nil, statErr
		}
		return nil, &attachmentError{tooLarge("attachment", int(info.Size()), maxBytes)}
	}
	return payload, nil
}

// ValueError of an attachment staging step
type attachmentError struct{ msg string }

func (e *attachmentError) Error() string      { return e.msg }
func (e *attachmentError) PyTypeName() string { return "ValueError" }

// Copies a browser upload receipt into this session under the browser cap
func stageBrowserFileAttachment(ctx context.Context, session *tuigateway.Session, stagedUpload any, name string) (string, bool, error) {
	rawHome, rawPath, err := stageduploads.ParseStagedUpload(ctx, stagedUpload)
	if err != nil {
		return "", false, err
	}
	source, err := stageduploads.ResolveStagedUpload(rawHome, rawPath)
	if err != nil {
		return "", false, err
	}
	limit := home.WebappAttachmentMaxBytes
	return stageSessionFileAttachment(ctx, session, source, "", name, &limit, true)
}

// Whether an attached host file is referenced where it is, inside the workspace and not a staged browser upload
func referencedInPlace(session *tuigateway.Session, path, workspace string, staged bool) bool {
	if _, inside := relativeTo(path, workspace); staged || !inside {
		return false
	}
	profileHome := strOr(session.Get("profile_home"))
	if profileHome == "" {
		profileHome = launchHome()
	}
	return !stageduploads.IsStagedUpload(profileHome, path)
}

// Makes a desktop file attachment visible to the agent as the stored path and whether it was copied
func stageSessionFileAttachment(ctx context.Context, session *tuigateway.Session, rawPath, dataURL, name string, maxBytes *int, staged bool) (string, bool, error) {
	workspace := resolveLax(sessionCwd(ctx, session))
	resolved := ""
	if rawPath != "" {
		if dropped := filedrop.DetectFileDrop(rawPath); dropped != nil {
			resolved = resolveLax(dropped.Path)
		} else {
			token, _ := filedrop.SplitPathInput(rawPath)
			if found := filedrop.ResolveAttachmentPath(token); found != "" {
				resolved = resolveLax(found)
			}
		}
	}
	var payload []byte
	filename := ""
	if resolved != "" {
		if referencedInPlace(session, resolved, workspace, staged) {
			return resolved, false, nil
		}
		if maxBytes != nil {
			data, err := readAttachmentBytes(resolved, *maxBytes)
			if err != nil {
				return "", false, err
			}
			payload = data
		}
		filename = pypath.Name(resolved)
	} else {
		if dataURL == "" {
			return "", false, &attachmentError{"file not found on gateway and no data_url provided"}
		}
		inlineMax := attachmentMaxBytes
		if maxBytes != nil {
			inlineMax = min(*maxBytes, attachmentMaxBytes)
		}
		data, err := b64Payload(dataURL, anyMediaDataURL, inlineMax, "attachment")
		var over *payloadTooLarge
		if errors.As(err, &over) {
			return "", false, err
		}
		if err != nil {
			return "", false, &attachmentError{"invalid data_url payload"}
		}
		payload = data
		if payload == nil {
			payload = []byte{}
		}
		source := name
		if source == "" {
			source = pypath.Name(rawPath)
		}
		filename = sanitizeAttachmentName(source)
	}
	root := sessionHomeDir(ctx, session, "attachments")
	if err := os.MkdirAll(root, 0o777); err != nil {
		return "", false, err
	}
	filename = sanitizeAttachmentName(filename)
	target := filepath.Join(root, filename)
	suffix := pypath.Suffix(filename)
	stem := strings.TrimSuffix(filename, suffix)
	if stem == "" {
		stem = "attachment"
	}
	var upload *os.File
	for counter := 2; ; counter++ {
		if _, err := os.Lstat(target); err == nil {
			target = filepath.Join(root, fmt.Sprintf("%s-%d%s", stem, counter, suffix))
			continue
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if errors.Is(err, fs.ErrExist) {
			target = filepath.Join(root, fmt.Sprintf("%s-%d%s", stem, counter, suffix))
			continue
		}
		if err != nil {
			return "", false, err
		}
		upload = f
		break
	}
	writeErr := func() error {
		if payload != nil {
			_, err := upload.Write(payload)
			return err
		}
		source, err := os.Open(resolved)
		if err != nil {
			return err
		}
		defer source.Close()
		_, err = io.CopyBuffer(upload, source, make([]byte, attachmentCopyChunkLen))
		return err
	}()
	if closeErr := upload.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = os.Remove(target)
		return "", false, writeErr
	}
	return resolveLax(target), true, nil
}
