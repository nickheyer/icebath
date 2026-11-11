package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Directory of files served below a mount, the Starlette StaticFiles
type StaticFiles struct {
	Directory     string
	HTML          bool
	FollowSymlink bool
	configChecked bool
	Override      func(ctx context.Context, s *StaticFiles, path string, scope *Scope) (ASGIResponse, error)
}

// Static files over a directory, refusing a missing one when checkDir is set
func NewStaticFiles(directory string, html, checkDir bool) (*StaticFiles, error) {
	if checkDir {
		if st, err := os.Stat(directory); err != nil || !st.IsDir() {
			return nil, &RuntimeError{Msg: fmt.Sprintf("Directory '%s' does not exist", directory)}
		}
	}
	return &StaticFiles{Directory: directory, HTML: html}, nil
}

// Serves the file the route path names, closing a websocket
func (s *StaticFiles) ASGI(ctx context.Context, scope *Scope, receive Receive, send Send) error {
	if scope.Type == "websocket" {
		return websocketClose(1000, "")(ctx, scope, receive, send)
	}
	if !s.configChecked {
		if err := s.checkConfig(); err != nil {
			return err
		}
		s.configChecked = true
	}
	path := s.getPath(scope)
	get := s.GetResponse
	if s.Override != nil {
		get = func(ctx context.Context, path string, scope *Scope) (ASGIResponse, error) {
			return s.Override(ctx, s, path, scope)
		}
	}
	resp, err := get(ctx, path, scope)
	if err != nil {
		return err
	}
	return resp.Serve(ctx, scope, receive, send)
}

// Normalised relative path of the route path with dot segments removed
func (s *StaticFiles) getPath(scope *Scope) string {
	parts := strings.Split(getRoutePath(scope), "/")
	joined := ""
	for _, p := range parts {
		if strings.HasPrefix(p, "/") || joined == "" {
			joined = p
		} else if strings.HasSuffix(joined, "/") {
			joined += p
		} else {
			joined += "/" + p
		}
	}
	return filepath.FromSlash(pypath.PosixNormpath(joined))
}

// File response, directory index, 404 page or the HTTP error a lookup ends in
func (s *StaticFiles) GetResponse(ctx context.Context, path string, scope *Scope) (ASGIResponse, error) {
	if scope.Method != "GET" && scope.Method != "HEAD" {
		return nil, NewHTTPException(405, nil, "Allow", "GET, HEAD")
	}
	full, st, err := s.lookupPath(path)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return nil, NewHTTPException(401, nil)
		}
		if errors.Is(err, syscall.ENAMETOOLONG) {
			return nil, NewHTTPException(404, nil)
		}
		return nil, err
	}
	if st != nil && st.Mode().IsRegular() {
		return s.fileResponse(full, st, scope, 200), nil
	}
	if st != nil && st.IsDir() && s.HTML {
		indexFull, ist, err := s.lookupPath(filepath.Join(path, "index.html"))
		if err != nil {
			return nil, err
		}
		if ist != nil && ist.Mode().IsRegular() {
			if !strings.HasSuffix(scope.Path, "/") {
				u := URLOfScope(scope)
				p := u.Path() + "/"
				return RedirectResponse(u.Replace(nil, nil, &p, nil).String(), 0), nil
			}
			return s.fileResponse(indexFull, ist, scope, 200), nil
		}
	}
	if s.HTML {
		full, st, err := s.lookupPath("404.html")
		if err != nil {
			return nil, err
		}
		if st != nil && st.Mode().IsRegular() {
			r := NewFileResponse(full, 404, "", nil, "")
			r.stat = st
			r.setStatHeaders(st)
			return r, nil
		}
	}
	return nil, NewHTTPException(404, nil)
}

// Full path and stat of a relative path inside the directory, empty when it escapes or is absent
func (s *StaticFiles) lookupPath(path string) (string, fs.FileInfo, error) {
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "\\") {
		return "", nil, nil
	}
	joined := filepath.Join(s.Directory, path)
	var full, dir string
	if s.FollowSymlink {
		full, _ = filepath.Abs(joined)
		dir, _ = filepath.Abs(s.Directory)
	} else {
		var err error
		if full, err = pypath.Resolve(joined); err != nil {
			return "", nil, err
		}
		if dir, err = pypath.Resolve(s.Directory); err != nil {
			return "", nil, err
		}
	}
	if full != dir && !strings.HasPrefix(full, strings.TrimRight(dir, string(filepath.Separator))+string(filepath.Separator)) {
		return "", nil, nil
	}
	st, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return "", nil, nil
		}
		return "", nil, err
	}
	return full, st, nil
}

// File response or a 304 when the request validators match it
func (s *StaticFiles) fileResponse(full string, st fs.FileInfo, scope *Scope, status int) ASGIResponse {
	r := NewFileResponse(full, status, "", nil, "")
	r.stat = st
	r.setStatHeaders(st)
	if isNotModified(r.Header(), headersOfScope(scope)) {
		var keep []string
		for _, kv := range r.Header().Items() {
			switch kv[0] {
			case "cache-control", "content-location", "date", "etag", "expires", "vary":
				keep = append(keep, kv[0], kv[1])
			}
		}
		return NewResponse(nil, 304, "", keep...)
	}
	return r
}

// Fails loudly when the directory is missing or not a directory
func (s *StaticFiles) checkConfig() error {
	st, err := os.Stat(s.Directory)
	if errors.Is(err, fs.ErrNotExist) {
		return &RuntimeError{Msg: fmt.Sprintf("StaticFiles directory '%s' does not exist.", s.Directory)}
	}
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return &RuntimeError{Msg: fmt.Sprintf("StaticFiles path '%s' is not a directory.", s.Directory)}
	}
	return nil
}

// Whether If-None-Match names the etag or If-Modified-Since is not older than the last modification
func isNotModified(resp, req Headers) bool {
	if inm := req.Get("if-none-match"); inm != "" {
		if pystr.Strip(inm) == "*" {
			return true
		}
		etag := resp.Get("etag")
		for _, tag := range strings.Split(inm, ",") {
			if strings.TrimPrefix(pystr.Strip(tag), "W/") == etag {
				return true
			}
		}
		return false
	}
	ims, ok := req.Lookup("if-modified-since")
	if !ok {
		return false
	}
	since, err1 := mail.ParseDate(ims)
	last, err2 := mail.ParseDate(resp.Get("last-modified"))
	if err1 != nil || err2 != nil {
		return false
	}
	a := [6]int{since.Year(), int(since.Month()), since.Day(), since.Hour(), since.Minute(), since.Second()}
	b := [6]int{last.Year(), int(last.Month()), last.Day(), last.Hour(), last.Minute(), last.Second()}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}
