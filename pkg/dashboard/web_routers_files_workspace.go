package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Adds the workspace filesystem routes the desktop file browser and spot editor use
func addWorkspaceFsRoutes(r *APIRouter, optStrQuery func(string) Param) {
	r.Get("/api/fs/list", &Endpoint{
		Name:   "fs_list",
		Params: []Param{RequiredQuery("path", Str), optStrQuery("profile")},
		Handle: func(ctx context.Context, c *Call) (any, error) { return fsList(ctx, c.Str("path"), c.Str("profile")) },
	})
	r.Get("/api/fs/read-text", &Endpoint{
		Name:   "fs_read_text",
		Params: []Param{RequiredQuery("path", Str), optStrQuery("profile"), optStrQuery("session_id")},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return fsReadText(ctx, c.Str("path"), c.Str("profile"), optionalStr(c, "session_id"))
		},
	})
	r.Post("/api/fs/write-text", &Endpoint{
		Name: "fs_write_text",
		Doc: "Overwrite (or create) a UTF-8 text file for the in-app spot editor.\n\n    Mirrors the Electron ``icebath:fs:writeText`` hardening: path validated by\n" +
			"    ``_fs_path``, the parent must already exist (never build trees), only\n    regular files may be replaced, payload size-capped, staged to a sibling\n" +
			"    temp file and ``os.replace``-d so a crash can't truncate the original.\n    Stale-on-disk detection is the client's job (re-read before save). The\n" +
			"    target resolves as ``read-text`` resolves it, ``session_id`` included, so a\n    file saves to the same place it was read from.\n    ",
		Params: []Param{optStrQuery("profile")},
		Body:   []BodyParam{Body("payload", fsWriteTextModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return fsWriteText(ctx, c.Object("payload"), c.Str("profile"))
		},
	})
	r.Get("/api/fs/read-data-url", &Endpoint{
		Name:   "fs_read_data_url",
		Params: []Param{RequiredQuery("path", Str), optStrQuery("profile"), optStrQuery("session_id")},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return fsReadDataURL(ctx, c.Request, c.Str("path"), c.Str("profile"), optionalStr(c, "session_id"))
		},
	})
	download := &Endpoint{
		Name: "fs_download",
		Doc: "A workspace file as an attachment: from the profile's SSH workspace when it has one, else\n" +
			"    from this host. A browser download link authenticates with a path-bound ``?ticket=``.",
		Params: []Param{RequiredQuery("path", Str), optStrQuery("profile"), optStrQuery("session_id")},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return fsDownload(ctx, c.Request, c.Str("path"), c.Str("profile"), optionalStr(c, "session_id"))
		},
	}
	r.AddAPIRoute("/api/fs/download", []string{"HEAD"}, download)
	r.AddAPIRoute("/api/fs/download", []string{"GET"}, download)
	r.Get("/api/fs/git-root", &Endpoint{
		Name:   "fs_git_root",
		Params: []Param{RequiredQuery("path", Str), optStrQuery("profile")},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return fsGitRoot(ctx, c.Str("path"), c.Str("profile"))
		},
	})
	r.Get("/api/fs/default-cwd", &Endpoint{
		Name:   "fs_default_cwd",
		Params: []Param{optStrQuery("profile")},
		Handle: func(ctx context.Context, c *Call) (any, error) { return fsDefaultCwdRoute(ctx, c.Str("profile")) },
	})
}

// Directory entries of a host folder, hidden build dirs and sensitive paths dropped, directories first
func fsListEntries(target string) ([]*pyjson.Object, error) {
	dirEntries, err := os.ReadDir(target)
	if err != nil {
		return nil, err
	}
	entries := []*pyjson.Object{}
	for _, entry := range dirEntries {
		entryPath := pypath.PathJoin(target, entry.Name())
		if fsReaddirHidden[entry.Name()] || tuigateway.IsSensitivePath(entryPath) {
			continue
		}
		entries = append(entries, pyjson.ObjectOf("name", entry.Name(), "path", entryPath, "isDirectory", entry.IsDir()))
	}
	sortFsEntries(entries)
	return entries, nil
}

// Folder listing of the profile workspace, an errno code in place of entries when it cannot be read
func fsList(ctx context.Context, path, profile string) (any, error) {
	backend, err := fsBackend(ctx, profile)
	if err != nil {
		return nil, err
	}
	if backend != nil {
		listing, err := backend.ListDir(path, fsReaddirHidden)
		if err != nil {
			return nil, fsBackendHTTPError(err)
		}
		return listing, nil
	}
	profileHome, err := fsProfileHome(ctx, profile)
	if err != nil {
		return nil, err
	}
	target, err := agentFsPath(ctx, path, nil, profileHome, "", true, false, "")
	if err != nil {
		return nil, err
	}
	if _, err := hostedFSReadGuard(target); err != nil {
		return nil, err
	}
	entries, err := fsListEntries(target)
	if err == nil {
		return map[string]any{"entries": entries}, nil
	}
	if !pypath.IsOSError(err) {
		return nil, err
	}
	code := "read-error"
	switch name, _ := pypath.ErrnoName(err); {
	case errors.Is(err, fs.ErrNotExist):
		code = "ENOENT"
	case name == "ENOTDIR":
		code = "ENOTDIR"
	case errors.Is(err, fs.ErrPermission):
		code = "EACCES"
	default:
		if text, ok := pypath.ErrnoText(err); ok && text != "" {
			code = text
		}
	}
	return map[string]any{"entries": []*pyjson.Object{}, "error": code}, nil
}

// Preview of a text file of the profile workspace, its first half megabyte decoded with replacement
func fsReadText(ctx context.Context, path, profile string, sessionID *string) (any, error) {
	session, err := fsOwnedSession(ctx, sessionID, profile)
	if err != nil {
		return nil, err
	}
	backend, err := remoteFsBackend(ctx, path, profile)
	if err != nil {
		return nil, err
	}
	previewLimit := int64(fsTextPreviewMaxBytes)
	if backend != nil {
		data, size, target, err := backend.ReadBytes(path, fsTextSourceMaxBytes, &previewLimit)
		if err != nil {
			return nil, fsBackendHTTPError(err)
		}
		return fsTextPreview(data, size, target), nil
	}
	profileHome, err := fsProfileHome(ctx, profile)
	if err != nil {
		return nil, err
	}
	target, err := fsDownloadPath(ctx, path, profile, sessionID, session, true, false)
	if err != nil {
		return nil, err
	}
	if err := hostGuard(ctx, target, profile); err != nil {
		return nil, err
	}
	target, st, herr := tuigateway.FsRegularFile(ctx, target, profileHome)
	if herr != nil {
		return nil, NewHTTPException(herr.Status, herr.Detail)
	}
	if st.Size() > fsTextSourceMaxBytes {
		return nil, NewHTTPException(413, "File too large")
	}
	limit := min(st.Size(), previewLimit)
	data, err := fsReadBytes(target, &limit)
	if err != nil {
		return nil, err
	}
	return fsTextPreview(data, st.Size(), target), nil
}

// Read guard of a host path unless it is one of the profile pastes or staged uploads
func hostGuard(ctx context.Context, target, profile string) error {
	host, err := isHostFile(ctx, target, profile)
	if err != nil || host {
		return err
	}
	_, err = hostedFSReadGuard(target)
	return err
}

// Text preview body of file bytes, its size and path
func fsTextPreview(data []byte, size int64, target string) map[string]any {
	language, ok := fsPreviewLanguageByExt[lowerSuffix(target)]
	if !ok {
		language = "text"
	}
	text, _ := pycodecs.DecodeReplace(data, "utf-8")
	return map[string]any{
		"binary":    fsLooksBinary(data[:min(len(data), 4096)]),
		"byteSize":  size,
		"language":  language,
		"mimeType":  fsMimeType(target),
		"path":      pypath.PathStr(target),
		"text":      text,
		"truncated": size > fsTextPreviewMaxBytes,
	}
}

// Overwrites or creates a text file for the spot editor through a sibling temp file
func fsWriteText(ctx context.Context, payload *pyjson.Object, profile string) (any, error) {
	text, _ := payload.GetOr("content", "").(string)
	path := objectStr(payload, "path")
	sessionID := objectOptStr(payload, "session_id")
	session, err := fsOwnedSession(ctx, sessionID, profile)
	if err != nil {
		return nil, err
	}
	backend, err := remoteFsBackend(ctx, path, profile)
	if err != nil {
		return nil, err
	}
	if backend != nil {
		target, byteSize, err := backend.WriteText(path, text, fsTextWriteMaxBytes)
		if err != nil {
			return nil, fsBackendHTTPError(err)
		}
		return pyjson.ObjectOf("ok", true, "path", target, "byteSize", byteSize), nil
	}
	target, err := fsDownloadPath(ctx, path, profile, sessionID, session, false, true)
	if err != nil {
		return nil, err
	}
	if len(text) > fsTextWriteMaxBytes {
		return nil, NewHTTPException(413, "Content too large")
	}
	st, err := os.Stat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		st = nil
	case errors.Is(err, fs.ErrPermission):
		return nil, NewHTTPException(403, "File is not writable")
	case err != nil:
		detail := pypath.OSErrorText(err)
		if detail == "" {
			detail = "Invalid path"
		}
		return nil, NewHTTPException(400, detail)
	}
	if st != nil && st.IsDir() {
		return nil, NewHTTPException(400, "Path points to a directory")
	}
	if st != nil && !st.Mode().IsRegular() {
		return nil, NewHTTPException(400, "Only regular files can be written")
	}
	if parent, _ := pypath.PathlibStat(pypath.PathParent(target)); parent == nil || !parent.IsDir() {
		return nil, NewHTTPException(400, "Parent directory does not exist")
	}
	tmp := pypath.PathJoin(pypath.PathParent(target), fmt.Sprintf(".%s.hermes-tmp-%d", pypath.PathName(target), os.Getpid()))
	err = os.WriteFile(tmp, []byte(text), 0o666)
	if err == nil {
		err = os.Rename(tmp, target)
	}
	if err != nil {
		_ = pypath.Unlink(tmp)
		if errors.Is(err, fs.ErrPermission) {
			return nil, NewHTTPException(403, "File is not writable")
		}
		return nil, NewHTTPException(500, "Could not write file: "+pypath.OSErrorText(err))
	}
	return pyjson.ObjectOf("ok", true, "path", target, "byteSize", len(text)), nil
}

// Host path a workspace route reads, a session id resolving container paths in its own sandbox
func fsDownloadPath(ctx context.Context, path, profile string, sessionID *string, session *pyjson.Object, decodeFallback, ownSandboxOnly bool) (string, error) {
	profileHome, err := fsProfileHome(ctx, profile)
	if err != nil {
		return "", err
	}
	if sessionID == nil {
		return agentFsPath(ctx, path, nil, profileHome, "", decodeFallback, false, "")
	}
	if session == nil {
		if session, err = fsOwnedSession(ctx, sessionID, profile); err != nil {
			return "", err
		}
	}
	cwd := ""
	if session != nil {
		if v, _ := session.GetOr("cwd", "").(string); v != "" {
			cwd = v
		}
	}
	workspaceKey, workspaceCwd, err := sessionWorkspace(tuigateway.AgentPathScope(ctx, profileHome), *sessionID, cwd)
	if err != nil {
		return "", err
	}
	return agentFsPath(ctx, path, &cwd, profileHome, workspaceKey, decodeFallback, ownSandboxOnly, workspaceCwd)
}

// Session the id names among the profile sessions, nil without an id and 404 for a foreign one
func fsOwnedSession(ctx context.Context, sessionID *string, profile string) (*pyjson.Object, error) {
	if sessionID == nil {
		return nil, nil
	}
	if pystr.Strip(*sessionID) == "" {
		return nil, NewHTTPException(404, "Session not found")
	}
	detail, err := getSessionDetail(ctx, *sessionID, profile)
	if err != nil {
		return nil, err
	}
	session, _ := detail.(*pyjson.Object)
	return session, nil
}

// Home of a requested profile, empty for the request own profile and 404 or 400 for an unknown one
func fsProfileHome(ctx context.Context, profile string) (string, error) {
	if isCurrentProfile(profile) {
		return "", nil
	}
	_, profileHome, err := cronProfileHome(ctx, profile)
	return profileHome, err
}

// Workspace file as a data URL, revalidated by an ETag over its identity
func fsReadDataURL(ctx context.Context, req *Request, path, profile string, sessionID *string) (any, error) {
	session, err := fsOwnedSession(ctx, sessionID, profile)
	if err != nil {
		return nil, err
	}
	backend, err := remoteFsBackend(ctx, path, profile)
	if err != nil {
		return nil, err
	}
	if backend != nil {
		data, _, target, err := backend.ReadBytes(path, fsDataURLMaxBytes, nil)
		if err != nil {
			return nil, fsBackendHTTPError(err)
		}
		return map[string]any{"dataUrl": "data:" + fsMimeType(target) + ";base64," + base64.StdEncoding.EncodeToString(data)}, nil
	}
	target, err := fsDownloadPath(ctx, path, profile, sessionID, session, true, false)
	if err != nil {
		return nil, err
	}
	if err := hostGuard(ctx, target, profile); err != nil {
		return nil, err
	}
	profileHome, err := fsProfileHome(ctx, profile)
	if err != nil {
		return nil, err
	}
	target, st, herr := tuigateway.FsRegularFile(ctx, target, profileHome)
	if herr != nil {
		return nil, NewHTTPException(herr.Status, herr.Detail)
	}
	if st.Size() > fsDataURLMaxBytes {
		return nil, NewHTTPException(413, "File too large")
	}
	etag, err := fsETag(target, st)
	if err != nil {
		return nil, err
	}
	validators := []string{"Cache-Control", "private, no-cache", "ETag", etag}
	tags := ifNoneMatch(req)
	if tags[etag] || tags["*"] {
		return NewResponse(nil, 304, "", validators...), nil
	}
	data, err := fsReadBytes(target, nil)
	if err != nil {
		return nil, err
	}
	body := pyjson.ObjectOf("dataUrl", "data:"+fsMimeType(target)+";base64,"+base64.StdEncoding.EncodeToString(data))
	return JSONResponse(body, 200, validators...)
}

// Validator that changes whenever the file at target does
func fsETag(target string, st os.FileInfo) (string, error) {
	dev, ino, err := pyfs.LstatIdentity(target)
	if err != nil {
		return "", err
	}
	identity := fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%d", target, dev, ino, st.Size(), st.ModTime().UnixNano())
	sum := sha256.Sum256([]byte(identity))
	return `"` + hex.EncodeToString(sum[:])[:32] + `"`, nil
}

// Entity tags of the If-None-Match header with weak prefixes dropped
func ifNoneMatch(req *Request) map[string]bool {
	tags := map[string]bool{}
	for _, tag := range strings.Split(req.Headers().Get("if-none-match"), ",") {
		tags[strings.TrimPrefix(pystr.Strip(tag), "W/")] = true
	}
	return tags
}

// Regular SSH workspace file a download may read and its size, 403 for a sensitive one
func remoteDownloadTarget(backend *SshWorkspaceFs, path string) (string, int64, error) {
	target, size, err := backend.InspectFile(path)
	if err != nil {
		var fsErr *SshWorkspaceFsError
		if errors.As(err, &fsErr) {
			return "", 0, fsBackendHTTPError(err)
		}
		return "", 0, err
	}
	if tuigateway.IsSensitivePath(target) {
		return "", 0, NewHTTPException(403, "Access to sensitive files is not allowed")
	}
	return target, size, nil
}

// SSH workspace media the stream route may serve and its size, 415 for a non media suffix
func remoteStreamTarget(backend *SshWorkspaceFs, path string) (string, int64, error) {
	target, size, err := remoteDownloadTarget(backend, path)
	if err != nil {
		return "", 0, err
	}
	if !streamableMediaExtensions[lowerPosixSuffix(target)] {
		return "", 0, NewHTTPException(415, "Unsupported media type")
	}
	return target, size, nil
}

// Ticket authority of a path on the profile SSH workspace checked by inspect, nil when this host owns it
func remoteFileAuthority(ctx context.Context, path, profile, owner string, inspect func(*SshWorkspaceFs, string) (string, int64, error)) (*FileAuthority, error) {
	backend, err := remoteFsBackend(ctx, path, profile)
	if err != nil || backend == nil {
		return nil, err
	}
	target, _, err := inspect(backend, path)
	if err != nil {
		return nil, err
	}
	authority := FileAuthority{Target: pypath.PosixPath(target), Homes: []string{owner}, Workspace: backend}
	if err := authority.CheckLive(); err != nil {
		return nil, err
	}
	return &authority, nil
}

// SSH file a ticket captured, rechecked by inspect while its profile is still live
func ticketedRemoteTarget(authority FileAuthority, inspect func(*SshWorkspaceFs, string) (string, int64, error)) (string, int64, error) {
	if err := authority.CheckLive(); err != nil {
		var missing *home.ErrProfileMissing
		if errors.As(err, &missing) || errors.Is(err, fs.ErrNotExist) {
			return "", 0, NewHTTPException(404, "File ticket profile or resource is unavailable")
		}
		return "", 0, err
	}
	backend, _ := authority.Workspace.(*SshWorkspaceFs)
	target, size, err := inspect(backend, authority.Target)
	if err != nil {
		return "", 0, err
	}
	if target != authority.Target {
		return "", 0, NewHTTPException(403, "File ticket target changed")
	}
	return target, size, nil
}

// SSH workspace file streamed as an attachment, a HEAD reading none of it
func remoteDownloadResponse(req *Request, backend *SshWorkspaceFs, target string, size int64) any {
	headers := []string{"Content-Disposition", "attachment; filename*=UTF-8''" + pyurl.Quote(pypath.PathName(target), "/")}
	mediaType := fsMimeType(target)
	if req != nil && req.Method() == "HEAD" {
		return NewResponse(nil, 200, mediaType, append(headers, "Content-Length", strconv.FormatInt(size, 10))...)
	}
	return NewStreamingResponse(backend.StreamFile(target, 64*1024, 0, nil), 200, mediaType, headers...)
}

// File a download ticket captures for this query, on the SSH workspace when the profile has one
func fsDownloadAuthority(ctx context.Context, req *Request, path, profile string, sessionID *string, owner string, session *pyjson.Object) (*FileAuthority, error) {
	remote, err := remoteFileAuthority(ctx, path, profile, owner, remoteDownloadTarget)
	if err != nil || remote != nil {
		return remote, err
	}
	local, err := fsDownloadPath(ctx, path, profile, sessionID, session, true, false)
	if err != nil {
		return nil, err
	}
	if _, err := hostedFSReadGuard(local); err != nil {
		return nil, err
	}
	profileHome, err := fsProfileHome(ctx, profile)
	if err != nil {
		return nil, err
	}
	target, _, herr := tuigateway.FsRegularFile(ctx, local, profileHome)
	if herr != nil {
		return nil, NewHTTPException(herr.Status, herr.Detail)
	}
	return liveFileAuthority(target, owner)
}

// Authority over a host file owned by owner and the named profile holding it, refused once either is gone
func liveFileAuthority(target, owner string) (*FileAuthority, error) {
	homes := []string{owner}
	if targetHome := home.NamedProfileHome(target); targetHome != "" && !containsPath(homes, targetHome) {
		homes = append(homes, targetHome)
	}
	authority := FileAuthority{Target: target, Homes: homes}
	if err := authority.CheckLive(); err != nil {
		return nil, err
	}
	return &authority, nil
}

// Whether paths holds p
func containsPath(paths []string, p string) bool {
	for _, q := range paths {
		if q == p {
			return true
		}
	}
	return false
}

// Serves the file a download ticket captured while its profile homes are still live
func ticketedFsDownload(ctx context.Context, req *Request, authority FileAuthority, profile string) (any, error) {
	if backend, ok := authority.Workspace.(*SshWorkspaceFs); ok && backend != nil {
		target, size, err := ticketedRemoteTarget(authority, remoteDownloadTarget)
		if err != nil {
			return nil, err
		}
		return remoteDownloadResponse(req, backend, target, size), nil
	}
	checked := func() (string, *int64, error) {
		if _, err := hostedFSReadGuard(authority.Target); err != nil {
			return "", nil, err
		}
		profileHome, err := fsProfileHome(ctx, profile)
		if err != nil {
			return "", nil, err
		}
		target, _, herr := tuigateway.FsRegularFile(ctx, authority.Target, profileHome)
		if herr != nil {
			return "", nil, NewHTTPException(herr.Status, herr.Detail)
		}
		return target, nil, nil
	}
	name := pypath.PathName(authority.Target)
	return NewOpenedFileResponse(authority.Target, 200, fsMimeType(authority.Target), &name, "attachment",
		func() (*os.File, error) { return pinTicketFile(authority, checked) }), nil
}

// Workspace file as an attachment from the profile SSH workspace or this host, a ticket pinning it
func fsDownload(ctx context.Context, req *Request, path, profile string, sessionID *string) (any, error) {
	session, err := fsOwnedSession(ctx, sessionID, profile)
	if err != nil {
		return nil, err
	}
	if authority, ok := req.State().Get("file_ticket_authority").(FileAuthority); ok {
		return ticketedFsDownload(ctx, req, authority, profile)
	}
	backend, err := remoteFsBackend(ctx, path, profile)
	if err != nil {
		return nil, err
	}
	if backend != nil {
		target, size, err := remoteDownloadTarget(backend, path)
		if err != nil {
			return nil, err
		}
		return remoteDownloadResponse(req, backend, target, size), nil
	}
	target, err := fsDownloadPath(ctx, path, profile, sessionID, session, true, false)
	if err != nil {
		return nil, err
	}
	if _, err := hostedFSReadGuard(target); err != nil {
		return nil, err
	}
	profileHome, err := fsProfileHome(ctx, profile)
	if err != nil {
		return nil, err
	}
	target, _, herr := tuigateway.FsRegularFile(ctx, target, profileHome)
	if herr != nil {
		return nil, NewHTTPException(herr.Status, herr.Detail)
	}
	if err := refuseLiveDatabase(target); err != nil {
		return nil, err
	}
	name := pypath.PathName(target)
	return NewFileResponse(target, 200, fsMimeType(target), &name, "attachment"), nil
}

// Nearest git root of a workspace path, null when none
func fsGitRoot(ctx context.Context, path, profile string) (any, error) {
	backend, err := fsBackend(ctx, profile)
	if err != nil {
		return nil, err
	}
	if backend != nil {
		root, ok, err := backend.GitRoot(path)
		if err != nil {
			return nil, fsBackendHTTPError(err)
		}
		if !ok {
			return map[string]any{"root": nil}, nil
		}
		return map[string]any{"root": root}, nil
	}
	target, herr := tuigateway.FsPath(path, nil, true)
	if herr != nil {
		return nil, NewHTTPException(herr.Status, herr.Detail)
	}
	start := target
	if st, err := os.Stat(target); err == nil && !st.IsDir() {
		start = pypath.PathParent(target)
	}
	if root, ok := fsFindGitRoot(start); ok {
		return map[string]any{"root": root}, nil
	}
	return map[string]any{"root": nil}, nil
}

// Folder and git branch a new session of the profile starts in
func fsDefaultCwdRoute(ctx context.Context, profile string) (any, error) {
	backend, err := fsBackend(ctx, profile)
	if err != nil {
		return nil, err
	}
	if backend != nil {
		cwd, err := backend.Cwd()
		if err != nil {
			return nil, err
		}
		branch, err := backend.GitBranch(cwd)
		if err != nil {
			return nil, fsBackendHTTPError(err)
		}
		return pyjson.ObjectOf("cwd", cwd, "branch", branch), nil
	}
	cwd, err := fsDefaultCwd(ctx, profile)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("cwd", cwd, "branch", fsGitBranch(cwd)), nil
}

// Lowercased suffix of a POSIX path the way PurePosixPath reads it
func lowerPosixSuffix(p string) string {
	name := pypath.PosixBasename(p)
	i := strings.LastIndex(name, ".")
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	return pystr.Lower(name[i:])
}
