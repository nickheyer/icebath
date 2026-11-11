package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var mediaProxyAllowedHosts = []string{"fal.media", "fal.run", "v3.fal.media", "storage.googleapis.com", "*.fal.media", "*.fal.run"}

const mediaProxyTimeout = 20 * time.Second

var (
	chatImageAllowedExtensions = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true}
	chatImageMagic             = [][2]string{{"\x89PNG\r\n\x1a\n", ".png"}, {"\xff\xd8\xff", ".jpg"}, {"GIF87a", ".gif"}, {"GIF89a", ".gif"}, {"BM", ".bmp"}}
	chatImageControlRE         = regexp.MustCompile(`[\x00-\x1f]+`)
	chatImageStemRE            = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
)

// Router of the media, chat image, managed files and workspace filesystem routes
func filesRouter() *APIRouter {
	r := NewAPIRouter("")
	optStrQuery := func(name string) Param { return QueryParam(name, Optional(Str), nil) }
	r.Get("/api/media", &Endpoint{
		Name: "get_media",
		Doc: `Return a gateway-local image as a base64 data URL for remote clients
    that can't read this machine's disk. Auth-gated; restricted to the image
    allowlist, a size cap AND the resolved (symlink-safe) media roots.`,
		Params: []Param{RequiredQuery("path", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getMedia(ctx, c.Str("path")) },
	})
	r.Get("/api/media/proxy", &Endpoint{
		Name: "proxy_remote_media",
		Doc: "Fetch a remote image URL the gateway can reach but the client cannot\n    (#74564), returning the same ``data_url`` shape as ``/api/media``. Only\n" +
			"    allowlisted image CDNs; the bytes stay behind the size cap.",
		Params: []Param{RequiredQuery("url", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return proxyRemoteMedia(ctx, c.Str("url"), c.Request) },
	})
	r.Post("/api/chat/image-upload", &Endpoint{
		Name: "upload_chat_image",
		Doc: "Persist a browser clipboard image where the embedded TUI can read it.\n\n    Browser clipboard bytes aren't visible to the server-side clipboard, so the\n" +
			"    /chat page uploads them here and drives the TUI's ``/image <path>`` with\n    the returned gateway-visible path under ``ICEBATH_HOME/images/`` (the same\n" +
			"    dir ``clipboard.paste`` / ``image.attach`` use).\n    ",
		Params: []Param{optStrQuery("profile")},
		Body:   []BodyParam{Body("payload", chatImageUploadModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return uploadChatImage(ctx, c.Object("payload"), c.Str("profile"))
		},
	})
	r.Get("/api/files", &Endpoint{
		Name:   "list_managed_files",
		Params: []Param{optStrQuery("path")},
		Handle: func(ctx context.Context, c *Call) (any, error) { return listManagedFiles(ctx, optionalStr(c, "path")) },
	})
	r.Get("/api/files/read", &Endpoint{
		Name:   "read_managed_file",
		Params: []Param{RequiredQuery("path", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return readManagedFile(ctx, c.Str("path")) },
	})
	r.Post("/api/files/ticket", &Endpoint{
		Name: "issue_file_ticket",
		Doc:  "Ticket a browser download/media URL instead of putting the session token in it.",
		Body: []BodyParam{Body("payload", fileTicketRequestModel)},
		Deps: []*Dependency{RouteOn("browser_files")},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return issueFileTicket(ctx, c.Object("payload"), c.Request)
		},
	})
	r.Get("/api/files/download", &Endpoint{
		Name: "download_managed_file",
		Doc: "Stream a managed file as an attachment download.\n\n    ``auth_middleware`` also accepts the session token as ``?token=`` here so a\n" +
			"    shell/browser-opened download (no session header) still authenticates.\n    Chromium marks ``<audio>``/``<video>`` subresource requests via\n" +
			"    ``Sec-Fetch-Dest``; those are served inline for Desktop builds that still\n    use this route as their player source, attachment semantics otherwise.\n    ",
		Params: []Param{RequiredQuery("path", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return downloadManagedFile(ctx, c.Request, c.Str("path"))
		},
	})
	stream := &Endpoint{
		Name: "stream_managed_file",
		Doc: "Stream managed audio/video inline with HTTP Range support — Electron's\n    media pipeline may reject an attachment response as an ``<audio>``/\n" +
			"    ``<video>`` source. Browser media elements authenticate with a reusable\n    path-bound ``?ticket=``; the session token is never accepted in this URL.\n" +
			"    No size cap (Range-served); sensitive guard and MIME detection as download. A\n    ``session_id`` resolves ``/workspace`` in that session's own sandbox, as the\n" +
			"    ticket route does. An SSH profile's clip streams from its SSH machine\n    (``_remote_fs_backend``, the rule download uses), one Range at a time.\n" +
			"    Every request checks its ``session_id`` first, a ticketed one included.",
		Params: []Param{RequiredQuery("path", Str), optStrQuery("profile"), optStrQuery("session_id")},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return streamManagedFile(ctx, c.Request, c.Str("path"), c.Str("profile"), optionalStr(c, "session_id"))
		},
	}
	r.AddAPIRoute("/api/files/stream", []string{"HEAD"}, stream)
	r.AddAPIRoute("/api/files/stream", []string{"GET"}, stream)
	r.Post("/api/files/upload", &Endpoint{
		Name:   "upload_managed_file",
		Body:   []BodyParam{Body("payload", managedFileUploadModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return uploadManagedFile(ctx, c.Object("payload")) },
	})
	r.Post("/api/files/upload-stream", &Endpoint{
		Name: "upload_managed_file_stream",
		Doc:  "Chunked multipart upload: constant memory and no base64 inflation, unlike\n    the JSON data-URL endpoint that trips proxy body-size limits on large archives.",
		Body: []BodyParam{FileField("file"), FormField("path", Str), FormFieldDefault("overwrite", Bool, true)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			file, _ := c.Value("file").(*UploadFile)
			return uploadManagedFileStream(ctx, file, c.Str("path"), c.Bool("overwrite"))
		},
	})
	r.Post("/api/files/mkdir", &Endpoint{
		Name: "create_managed_directory",
		Body: []BodyParam{Body("payload", managedDirectoryCreateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return createManagedDirectory(ctx, c.Object("payload"))
		},
	})
	r.Delete("/api/files", &Endpoint{
		Name:   "delete_managed_file",
		Body:   []BodyParam{Body("payload", managedFileDeleteModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return deleteManagedFile(ctx, c.Object("payload")) },
	})
	addWorkspaceFsRoutes(r, optStrQuery)
	return r
}

// Optional string value of a call, nil when the request omitted it
func optionalStr(c *Call, name string) *string {
	if s, ok := c.Value(name).(string); ok {
		return &s
	}
	return nil
}

// Optional string field of a validated body, nil when absent or null
func objectOptStr(o *pyjson.Object, key string) *string {
	if s, ok := o.GetOr(key, nil).(string); ok {
		return &s
	}
	return nil
}

// Object string field of a validated body
func objectStr(o *pyjson.Object, key string) string {
	s, _ := o.GetOr(key, "").(string)
	return s
}

// Data URL of an allowlisted image under the media roots, 400 415 403 404 or 413 when refused
func getMedia(ctx context.Context, path string) (any, error) {
	expanded, err := pypath.PathExpanduser(path)
	if err != nil {
		return nil, NewHTTPException(400, "Invalid path")
	}
	target, err := pypath.Resolve(expanded)
	if err != nil {
		return nil, NewHTTPException(400, "Invalid path")
	}
	contentType, ok := mediaContentTypes[lowerSuffix(target)]
	if !ok {
		return nil, NewHTTPException(415, "Unsupported media type")
	}
	inRoot := false
	for _, root := range mediaServeRoots(ctx) {
		if pathIsUnder(root, target) {
			inRoot = true
			break
		}
	}
	if !inRoot {
		return nil, NewHTTPException(403, "Path outside media roots")
	}
	fi, err := pypath.PathlibStat(target)
	if err != nil {
		return nil, err
	}
	if fi == nil || !fi.Mode().IsRegular() {
		return nil, NewHTTPException(404, "File not found")
	}
	if fi.Size() > mediaMaxBytes {
		return nil, NewHTTPException(413, "File too large")
	}
	encoded, err := readBase64File(target)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("data_url", "data:"+contentType+";base64,"+encoded), nil
}

// Whether a host is an allowlisted image CDN, exact or by wildcard subdomain
func mediaProxyHostAllowed(host string) bool {
	host = strings.TrimRight(pystr.Lower(host), ".")
	for _, allowed := range mediaProxyAllowedHosts {
		if host == strings.TrimLeft(allowed, "*.") || (strings.HasPrefix(allowed, "*.") && strings.HasSuffix(host, allowed[1:])) {
			return true
		}
	}
	return false
}

// Data URL of a remote allowlisted image fetched by the gateway, under the media size cap
func proxyRemoteMedia(ctx context.Context, rawURL string, req *Request) (any, error) {
	if err := requireToken(req); err != nil {
		return nil, err
	}
	parsed, err := pyurl.Parse(pystr.Strip(rawURL))
	if err != nil {
		return nil, NewHTTPException(400, "A remote image URL is required")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return nil, NewHTTPException(400, "A remote image URL is required")
	}
	if !mediaProxyHostAllowed(parsed.Hostname()) {
		return nil, NewHTTPException(403, "Image host not allowed")
	}
	suffix := lowerSuffix(parsed.Path)
	if _, known := mediaContentTypes[suffix]; parsed.Path == "" || !known {
		if pypath.PathSuffix(parsed.Path) != "" && !known {
			return nil, NewHTTPException(415, "Unsupported media type")
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, NewHTTPException(502, "Image fetch failed: "+pyhttpx.ErrorText(err))
	}
	response, err := pyhttpx.Do(pyhttpx.PlainClient(true), request, mediaProxyTimeout)
	if err != nil {
		return nil, NewHTTPException(502, "Image fetch failed: "+pyhttpx.ErrorText(err))
	}
	if response.StatusCode != 200 {
		return nil, NewHTTPException(502, fmt.Sprintf("Image fetch returned HTTP %d", response.StatusCode))
	}
	contentType := pystr.Lower(pystr.Strip(strings.SplitN(response.Header.Get("content-type"), ";", 2)[0]))
	allowed := false
	for _, v := range mediaContentTypes {
		if v == contentType {
			allowed = true
		}
	}
	if !allowed {
		return nil, NewHTTPException(415, "Unsupported media type")
	}
	if len(response.Content) > mediaMaxBytes {
		return nil, NewHTTPException(413, "File too large")
	}
	return pyjson.ObjectOf("data_url", "data:"+contentType+";base64,"+base64.StdEncoding.EncodeToString(response.Content)), nil
}

// Bytes and media type of a base64 data URL upload, 400 when malformed and 413 past the cap
func decodeDataURL(dataURL string) ([]byte, string, error) {
	text := pystr.Strip(dataURL)
	if !strings.HasPrefix(text, "data:") || !strings.Contains(text, ",") {
		return nil, "", NewHTTPException(400, "Upload payload must be a data URL")
	}
	header, encoded, _ := strings.Cut(text, ",")
	mimeType := strings.SplitN(header[5:], ";", 2)[0]
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	if !strings.Contains(header, ";base64") {
		return nil, "", NewHTTPException(400, "Upload payload must be base64 encoded")
	}
	data, err := pybase64.B64DecodeValidate(encoded)
	if err != nil {
		return nil, "", NewHTTPException(400, "Upload payload is not valid base64")
	}
	if len(data) > managedFileMaxBytes {
		return nil, "", NewHTTPException(413, "File is too large")
	}
	return data, mimeType, nil
}

// Basename of a pasted image name with control runs replaced, pasted-image when nothing remains
func sanitizeChatImageFilename(filename *string) string {
	raw := ""
	if filename != nil {
		raw = *filename
	}
	candidate := pypath.PathName(pystr.Strip(raw))
	candidate = chatImageControlRE.ReplaceAllString(candidate, "_")
	candidate = strings.Trim(pystr.Strip(candidate), ".")
	if candidate == "" {
		return "pasted-image"
	}
	return candidate
}

// Image extension the leading bytes declare, empty for an unknown format
func chatImageExtension(data []byte) string {
	head := string(data[:min(len(data), 16)])
	if strings.HasPrefix(head, "RIFF") && len(head) >= 12 && head[8:12] == "WEBP" {
		return ".webp"
	}
	for _, magic := range chatImageMagic {
		if strings.HasPrefix(head, magic[0]) {
			return magic[1]
		}
	}
	return ""
}

// Bytes, media type and extension of a pasted image upload, 400 or 413 when refused
func decodeChatImageUpload(payload *pyjson.Object) ([]byte, string, string, error) {
	data, mimeType, err := decodeDataURL(objectStr(payload, "data_url"))
	if err != nil {
		return nil, "", "", err
	}
	if !strings.HasPrefix(pystr.Lower(mimeType), "image/") {
		return nil, "", "", NewHTTPException(400, "Upload payload must be an image")
	}
	if len(data) > ChatImageMaxBytes {
		return nil, "", "", NewHTTPException(413, fmt.Sprintf("Image is too large; cap is %d MB", ChatImageMaxBytes/(1024*1024)))
	}
	ext := chatImageExtension(data)
	if !chatImageAllowedExtensions[ext] {
		return nil, "", "", NewHTTPException(400, "Unsupported image type")
	}
	return data, mimeType, ext, nil
}

// Persists a browser clipboard image under the profile image cache where the embedded TUI reads it
func uploadChatImage(ctx context.Context, payload *pyjson.Object, profile string) (any, error) {
	data, mimeType, ext, err := decodeChatImageUpload(payload)
	if err != nil {
		return nil, err
	}
	h, err := resolveUploadHome(ctx, profile)
	if err != nil {
		return nil, err
	}
	rel, err := pypath.Relpath(chatImageDir(h), h)
	if err != nil {
		return nil, err
	}
	imgDir, err := ensureProfileSubdir(ctx, h, rel, "Image directory is not writable", "Could not create image directory", nil)
	if err != nil {
		return nil, err
	}
	home.ApplySecureDirPolicy(ctx, imgDir, h)
	stem := pypath.PathStem(sanitizeChatImageFilename(objectOptStr(payload, "filename")))
	if stem == "" {
		stem = "pasted-image"
	}
	stem = strings.Trim(chatImageStemRE.ReplaceAllString(stem, "_"), "._-")
	if stem == "" {
		stem = "pasted-image"
	}
	token := make([]byte, 4)
	_, _ = rand.Read(token)
	target := pypath.PathJoin(imgDir, fmt.Sprintf("dashboard_%s_%s_%s%s", time.Now().Format("20060102_150405"), hex.EncodeToString(token), stem, ext))
	err = removedOnFailure(target, func() error {
		if err := ioErrors("Image directory is not writable", "Could not write image", os.WriteFile(target, data, 0o666)); err != nil {
			return err
		}
		if err := home.AssertProfileLive(h); err != nil {
			return NewHTTPException(404, "Profile was deleted during upload")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "path", target, "name", pypath.PathName(target), "bytes", len(data), "mime_type", mimeType), nil
}

// Merged object of a response body and the managed policy fields
func withManagedMeta(o *pyjson.Object, policy managedFilesPolicy) *pyjson.Object {
	o.Update(managedResponseMeta(policy))
	return o
}

// Managed directory listing without sensitive entries, directories first then by lowercased name
func listManagedFiles(ctx context.Context, path *string) (any, error) {
	policy, target, displayPath, err := resolveManagedPath(ctx, path, false, true, false)
	if err != nil {
		return nil, err
	}
	fi, err := pypath.PathlibStat(target)
	if err != nil {
		return nil, err
	}
	if fi == nil {
		return nil, NewHTTPException(404, "Path not found")
	}
	if !fi.IsDir() {
		return nil, NewHTTPException(400, "Path is not a directory")
	}
	dirEntries, err := os.ReadDir(target)
	if err != nil {
		return nil, ioErrors("Directory is not readable", "Could not read directory", err)
	}
	entries := []*pyjson.Object{}
	for _, entry := range dirEntries {
		entryPath := pypath.PathJoin(target, entry.Name())
		if tuigateway.IsSensitivePath(entryPath) {
			continue
		}
		metadata, err := managedFileEntry(policy, entryPath, true)
		if err != nil {
			return nil, err
		}
		if metadata != nil {
			entries = append(entries, metadata)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		di, _ := entries[i].GetOr("is_directory", false).(bool)
		dj, _ := entries[j].GetOr("is_directory", false).(bool)
		if di != dj {
			return di
		}
		ni, _ := entries[i].GetOr("name", "").(string)
		nj, _ := entries[j].GetOr("name", "").(string)
		return pystr.Lower(ni) < pystr.Lower(nj)
	})
	var parent any
	if p := pypath.PathParent(target); p != target && (policy.lockedRoot == "" || target != policy.lockedRoot) {
		parent = p
	}
	return withManagedMeta(pyjson.ObjectOf("path", displayPath, "parent", parent, "entries", entries), policy), nil
}

// Policy, target, display path, size cap and media type of a readable managed file, 404 400 or 403 when refused
func managedReadableFile(ctx context.Context, path string, createRoot bool) (managedFilesPolicy, string, string, int64, string, error) {
	policy, target, displayPath, err := resolveManagedPath(ctx, &path, false, createRoot, true)
	if err != nil {
		return policy, "", "", 0, "", err
	}
	fi, err := pypath.PathlibStat(target)
	if err != nil {
		return policy, "", "", 0, "", err
	}
	if fi == nil {
		return policy, "", "", 0, "", NewHTTPException(404, "File not found")
	}
	if !fi.Mode().IsRegular() {
		return policy, "", "", 0, "", NewHTTPException(400, "Path is not a file")
	}
	if tuigateway.IsSensitivePath(target) {
		return policy, "", "", 0, "", NewHTTPException(403, "Access to sensitive files is not allowed")
	}
	mimeType, _, _ := pymimetypes.GuessType(pypath.PathName(target), true)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return policy, target, displayPath, managedFileMaxBytes, mimeType, nil
}

// Size of a managed file, 500 when it cannot be read and 413 past the cap
func managedFileSize(target string, maxBytes int64) (int64, error) {
	fi, err := os.Stat(target)
	if err != nil {
		return 0, NewHTTPException(500, "Could not stat file: "+pypath.OSErrorText(err))
	}
	if fi.Size() > maxBytes {
		return 0, NewHTTPException(413, "File is too large")
	}
	return fi.Size(), nil
}

// Managed file as a data URL with its metadata and the policy fields
func readManagedFile(ctx context.Context, path string) (any, error) {
	policy, target, displayPath, maxBytes, mimeType, err := managedReadableFile(ctx, path, true)
	if err != nil {
		return nil, err
	}
	size, err := managedFileSize(target, maxBytes)
	if err != nil {
		return nil, err
	}
	encoded, err := readBase64File(target)
	if err = ioErrors("File is not readable", "Could not read file", err); err != nil {
		return nil, err
	}
	body := withManagedMeta(pyjson.ObjectOf("name", pypath.PathName(target), "path", displayPath, "size", size,
		"mime_type", mimeType, "data_url", "data:"+mimeType+";base64,"+encoded), policy)
	return JSONResponse(body, 200)
}

// Range aware managed file response, a ticketed request served from the file its ticket pinned
func managedFileResponse(ctx context.Context, req *Request, path, dispositionType string, mediaOnly bool) (any, error) {
	var headers []string
	if mediaOnly {
		headers = []string{"X-Content-Type-Options", "nosniff"}
	}
	if authority, ok := req.State().Get("file_ticket_authority").(FileAuthority); ok {
		name := pypath.PathName(authority.Target)
		return NewOpenedFileResponse(authority.Target, 200, fsMimeType(authority.Target), &name, dispositionType,
			func() (*os.File, error) { return openTicketFile(ctx, authority, mediaOnly) }, headers...), nil
	}
	_, target, _, maxBytes, mimeType, err := managedReadableFile(ctx, path, true)
	if err != nil {
		return nil, err
	}
	if mediaOnly && !streamableMediaExtensions[lowerSuffix(target)] {
		return nil, NewHTTPException(415, "Unsupported media type")
	}
	if !mediaOnly {
		if _, err := managedFileSize(target, maxBytes); err != nil {
			return nil, err
		}
	}
	if err := refuseLiveDatabase(target); err != nil {
		return nil, err
	}
	name := pypath.PathName(target)
	return NewFileResponse(target, 200, mimeType, &name, dispositionType, headers...), nil
}

// Opens the managed file a ticket names once its profile homes are live and the read rules still admit it
func openTicketFile(ctx context.Context, authority FileAuthority, mediaOnly bool) (*os.File, error) {
	return pinTicketFile(authority, func() (string, *int64, error) {
		_, target, _, maxBytes, _, err := managedReadableFile(ctx, authority.Target, false)
		if err != nil {
			return "", nil, err
		}
		if mediaOnly && !streamableMediaExtensions[lowerSuffix(target)] {
			return "", nil, NewHTTPException(415, "Unsupported media type")
		}
		if mediaOnly {
			return target, nil, nil
		}
		return target, &maxBytes, nil
	})
}

// Opens the host file a ticket names after checked returns that same file and its size cap
func pinTicketFile(authority FileAuthority, checked func() (string, *int64, error)) (*os.File, error) {
	handle, err := func() (*os.File, error) {
		if err := authority.CheckLive(); err != nil {
			return nil, err
		}
		target, maxBytes, err := checked()
		if err != nil {
			return nil, err
		}
		if target != authority.Target {
			return nil, NewHTTPException(403, "File ticket target changed")
		}
		if err := refuseLiveDatabase(target); err != nil {
			return nil, err
		}
		handle, err := os.Open(target)
		if err != nil {
			return nil, err
		}
		opened, err := handle.Stat()
		if err != nil {
			handle.Close()
			return nil, err
		}
		if !opened.Mode().IsRegular() {
			handle.Close()
			return nil, NewHTTPException(400, "Only regular files can be read")
		}
		if maxBytes != nil && opened.Size() > *maxBytes {
			handle.Close()
			return nil, NewHTTPException(413, "File is too large")
		}
		return handle, nil
	}()
	if err == nil {
		return handle, nil
	}
	var missing *home.ErrProfileMissing
	if errors.Is(err, fs.ErrNotExist) || errors.As(err, &missing) {
		return nil, NewHTTPException(404, "File ticket profile or resource is unavailable")
	}
	if errors.Is(err, fs.ErrPermission) {
		return nil, NewHTTPException(403, "File is not readable")
	}
	return nil, err
}

// Managed file download, served inline as media for an audio or video subresource request
func downloadManagedFile(ctx context.Context, req *Request, path string) (any, error) {
	dest := pystr.Lower(req.Headers().Get("sec-fetch-dest"))
	media := dest == "audio" || dest == "video"
	disposition := "attachment"
	if media {
		disposition = "inline"
	}
	return managedFileResponse(ctx, req, path, disposition, media)
}

// Write target of a managed path, 409 for a directory there or an existing file without overwrite
func managedWriteTarget(ctx context.Context, path string, overwrite bool) (managedFilesPolicy, string, string, error) {
	policy, target, displayPath, err := resolveManagedPath(ctx, &path, true, true, false)
	if err != nil {
		return policy, "", "", err
	}
	fi, err := pypath.PathlibStat(target)
	if err != nil {
		return policy, "", "", err
	}
	if fi != nil && fi.IsDir() {
		return policy, "", "", NewHTTPException(409, "A directory already exists at that path")
	}
	if fi != nil && !overwrite {
		return policy, "", "", NewHTTPException(409, "File already exists")
	}
	return policy, target, displayPath, nil
}

// Write result of a managed path, its entry, display path and the policy fields
func managedWriteResult(policy managedFilesPolicy, target, displayPath string) (any, error) {
	entry, err := managedFileEntry(policy, target, false)
	if err != nil {
		return nil, err
	}
	return withManagedMeta(pyjson.ObjectOf("ok", true, "entry", entry, "path", displayPath), policy), nil
}

// Writes a data URL upload to a managed path, parents created
func uploadManagedFile(ctx context.Context, payload *pyjson.Object) (any, error) {
	overwrite, _ := payload.GetOr("overwrite", true).(bool)
	policy, target, displayPath, err := managedWriteTarget(ctx, objectStr(payload, "path"), overwrite)
	if err != nil {
		return nil, err
	}
	data, _, err := decodeDataURL(objectStr(payload, "data_url"))
	if err != nil {
		return nil, err
	}
	err = os.MkdirAll(pypath.PathParent(target), 0o777)
	if err == nil {
		err = os.WriteFile(target, data, 0o666)
	}
	if err = ioErrors("File is not writable", "Could not write file", err); err != nil {
		return nil, err
	}
	return managedWriteResult(policy, target, displayPath)
}

// Streams a multipart upload to target through a sibling temp file, 413 past the managed cap
func streamUploadToPath(file *UploadFile, target, tooLarge, notWritable, writeFailed string) (int64, error) {
	tmp, err := os.CreateTemp(pypath.PathParent(target), "."+pypath.PathName(target)+".*.upload")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	var total int64
	renamed := false
	defer func() {
		if !renamed {
			_ = pypath.Unlink(tmpName)
		}
		_ = file.Close()
	}()
	err = func() error {
		defer tmp.Close()
		for {
			chunk, err := file.Read(uploadChunkBytes)
			if err != nil {
				return err
			}
			if len(chunk) == 0 {
				return nil
			}
			total += int64(len(chunk))
			if total > managedFileMaxBytes {
				return NewHTTPException(413, tooLarge)
			}
			if _, err := tmp.Write(chunk); err != nil {
				return err
			}
		}
	}()
	if err == nil {
		if err = os.Rename(tmpName, target); err == nil {
			renamed = true
		}
	}
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return 0, NewHTTPException(403, notWritable)
		}
		if pypath.IsOSError(err) {
			return 0, NewHTTPException(500, writeFailed+": "+pypath.OSErrorText(err))
		}
		return 0, err
	}
	return total, nil
}

// Chunked multipart upload to a managed path with constant memory
func uploadManagedFileStream(ctx context.Context, file *UploadFile, path string, overwrite bool) (any, error) {
	policy, target, displayPath, err := managedWriteTarget(ctx, path, overwrite)
	if err != nil {
		return nil, err
	}
	if err := ioErrors("File is not writable", "Could not create parent directory", os.MkdirAll(pypath.PathParent(target), 0o777)); err != nil {
		return nil, err
	}
	if _, err := streamUploadToPath(file, target, "File is too large", "File is not writable", "Could not write file"); err != nil {
		return nil, err
	}
	return managedWriteResult(policy, target, displayPath)
}

// Creates a managed directory and its parents, 409 when a file stands there
func createManagedDirectory(ctx context.Context, payload *pyjson.Object) (any, error) {
	path := objectStr(payload, "path")
	policy, target, displayPath, err := resolveManagedPath(ctx, &path, true, true, false)
	if err != nil {
		return nil, err
	}
	fi, err := pypath.PathlibStat(target)
	if err != nil {
		return nil, err
	}
	if fi != nil && !fi.IsDir() {
		return nil, NewHTTPException(409, "A file already exists at that path")
	}
	if err := ioErrors("Directory is not writable", "Could not create directory", os.MkdirAll(target, 0o777)); err != nil {
		return nil, err
	}
	return managedWriteResult(policy, target, displayPath)
}

// Deletes a managed file or directory, never the managed root or the filesystem root
func deleteManagedFile(ctx context.Context, payload *pyjson.Object) (any, error) {
	path := objectStr(payload, "path")
	recursive, _ := payload.GetOr("recursive", false).(bool)
	policy, target, displayPath, err := resolveManagedPath(ctx, &path, false, true, false)
	if err != nil {
		return nil, err
	}
	if policy.lockedRoot != "" && target == policy.lockedRoot {
		return nil, NewHTTPException(400, "Cannot delete the managed files root")
	}
	if pypath.PathParent(target) == target {
		return nil, NewHTTPException(400, "Cannot delete the filesystem root")
	}
	fi, err := pypath.PathlibStat(target)
	if err != nil {
		return nil, err
	}
	if fi == nil {
		return nil, NewHTTPException(404, "Path not found")
	}
	isDir := fi.IsDir()
	switch {
	case isDir && recursive:
		err = pyshutil.Rmtree(target)
	case isDir:
		err = os.Remove(target)
	default:
		err = pypath.Unlink(target)
	}
	if err != nil {
		status := 500
		if after, _ := pypath.PathlibStat(target); after != nil && after.IsDir() && !recursive {
			status = 409
		}
		return nil, NewHTTPException(status, "Could not delete path: "+pypath.OSErrorText(err))
	}
	return withManagedMeta(pyjson.ObjectOf("ok", true, "path", displayPath), policy), nil
}
