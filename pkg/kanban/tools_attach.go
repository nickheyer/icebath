package kanban

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const maxAttachURLRedirects = 5

// Client the attachment download sends each hop with, redirects returned to the caller
var attachHTTPClient = func() *http.Client { return pyhttpx.PlainClient(false) }

// Capped download the attach_url handler runs, swapped by tests like the Python module attribute
var downloadURLWithCapFn = downloadURLWithCap

// Stores attachment bytes through the shared board writer and reports the new row
func storeAttachment(k *kanbanCall, tid string, filename any, data []byte, contentType any) (string, error) {
	board, _ := boardArg(k.get("board"))
	return withBoard(k.ctx, k.get("board"), false, func(conn state.SQLiteConn) (string, error) {
		mime, bindErr := sqliteBind(contentType, 4)
		uploader := "agent"
		id, err := StoreAttachmentBytes(k.ctx, conn, tid, pyrepr.Str(filename), data,
			StoreAttachmentOptions{ContentType: mime, UploadedBy: &uploader, Board: board, contentTypeBindErr: bindErr})
		if err != nil {
			return "", err
		}
		return okPayload("task_id", tid, "attachment_id", id, "size", int64(len(data)))
	})
}

// Attaches a file whose bytes arrive inline as base64
func handleAttach(k *kanbanCall) (string, error) {
	tid, err := workerGuard(k, "kanban_attach")
	if err != nil {
		return "", err
	}
	filename, err := requireText(k, "filename", "")
	if err != nil {
		return "", err
	}
	content, err := requireText(k, "content_base64", "")
	if err != nil {
		return "", err
	}
	data, err := pybase64.B64DecodeValidate(pyrepr.Str(content))
	if err != nil {
		return "", reject(fmt.Sprintf("content_base64 is not valid base64: %s", err.Error()))
	}
	return storeAttachment(k, tid, filename, data, k.get("content_type"))
}

// Fetches a URL capped at maxBytes with every hop checked against private and internal targets
func downloadURLWithCap(ctx context.Context, env *tools.Env, rawURL string, maxBytes int64) ([]byte, *string, error) {
	currentURL := rawURL
	for range maxAttachURLRedirects + 1 {
		parsed, err := pyurl.Parse(currentURL)
		if err != nil {
			return nil, nil, &ValueError{err.Error()}
		}
		scheme := pystr.Lower(parsed.Scheme)
		if scheme != "http" && scheme != "https" {
			return nil, nil, &ValueError{fmt.Sprintf("unsupported URL scheme %s; only http/https are allowed", pyrepr.StrRepr(scheme))}
		}
		if !tools.IsSafeURL(env, currentURL) {
			return nil, nil, &ValueError{fmt.Sprintf("URL blocked by SSRF protection (private/internal address): %s", currentURL)}
		}
		data, contentType, next, err := fetchAttachmentHop(ctx, currentURL, maxBytes)
		if err != nil {
			return nil, nil, err
		}
		if next == "" {
			return data, contentType, nil
		}
		currentURL = next
	}
	return nil, nil, &ValueError{fmt.Sprintf("too many redirects fetching %s", rawURL)}
}

// One GET of the download, returning the body and content type or the location a redirect points at
func fetchAttachmentHop(ctx context.Context, currentURL string, maxBytes int64) ([]byte, *string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, currentURL, nil)
	if err != nil {
		return nil, nil, "", &RuntimeError{pyhttpx.ErrorText(err)}
	}
	req.Header.Set("User-Agent", "icebath-kanban/attach")
	req.Header.Set("Accept", "*/*")
	resp, err := pyhttpx.Stream(attachHTTPClient(), req, 30*time.Second)
	if err != nil {
		return nil, nil, "", &RuntimeError{pyhttpx.ErrorText(err)}
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		if locations := resp.Header.Values("Location"); len(locations) > 0 {
			location := strings.Join(locations, ", ")
			if location == "" {
				return nil, nil, "", &ValueError{fmt.Sprintf("redirect without Location header from %s", currentURL)}
			}
			next, err := pyurl.Join(currentURL, location)
			if err != nil {
				return nil, nil, "", &ValueError{err.Error()}
			}
			return nil, nil, next, nil
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, nil, "", &RuntimeError{resp.StatusErrorText(currentURL)}
	}
	var contentType *string
	if first, _, _ := strings.Cut(strings.Join(resp.Header.Values("Content-Type"), ", "), ";"); pystr.Strip(first) != "" {
		stripped := pystr.Strip(first)
		contentType = &stripped
	}
	var data []byte
	chunk := make([]byte, 1024*1024)
	for {
		n, err := resp.Body.Read(chunk)
		if n > 0 {
			if int64(len(data)+n) > maxBytes {
				return nil, nil, "", &ValueError{fmt.Sprintf("attachment exceeds %d MB limit", maxBytes/(1024*1024))}
			}
			data = append(data, chunk[:n]...)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, "", &RuntimeError{pyhttpx.ErrorText(err)}
		}
	}
	if data == nil {
		data = []byte{}
	}
	return data, contentType, "", nil
}

// Attaches a file fetched server side from an http or https URL under the shared size cap
func handleAttachURL(k *kanbanCall) (string, error) {
	tid, err := workerGuard(k, "kanban_attach_url")
	if err != nil {
		return "", err
	}
	rawURL, err := requireText(k, "url", "")
	if err != nil {
		return "", err
	}
	url := pystr.Strip(pyrepr.Str(rawURL))
	filename := k.get("filename")
	if !pyrepr.Truthy(filename) {
		filename = k.get("title")
	}
	if !pyrepr.Truthy(filename) || pystr.Strip(pyrepr.Str(filename)) == "" {
		parsed, err := pyurl.Parse(url)
		if err != nil {
			return "", &ValueError{err.Error()}
		}
		leaf := parsed.Path
		if i := strings.LastIndex(leaf, "/"); i >= 0 {
			leaf = leaf[i+1:]
		}
		name := pystr.Strip(pyurl.Unquote(leaf))
		if name == "" {
			name = "download"
		}
		filename = name
	}
	data, fetchedType, err := downloadURLWithCapFn(k.ctx, k.env, url, KanbanAttachmentMaxBytes)
	if err != nil {
		if isValueError(err) {
			return kanbanToolError(fmt.Sprintf("kanban_attach_url: %s", excText(err))), nil
		}
		kanbanToolsLog.Error("kanban_attach_url download failed", "exc_info", excText(err))
		return kanbanToolError(fmt.Sprintf("kanban_attach_url: failed to fetch %s: %s", url, excText(err))), nil
	}
	contentType := k.get("content_type")
	if !pyrepr.Truthy(contentType) {
		contentType = optStrValue(fetchedType)
	}
	return storeAttachment(k, tid, filename, data, contentType)
}

// Lists the attachments of a task, readable by any caller that names or owns one
func handleAttachments(k *kanbanCall) (string, error) {
	tid, err := requireTaskID(k)
	if err != nil {
		return "", err
	}
	return withBoard(k.ctx, k.get("board"), false, func(conn state.SQLiteConn) (string, error) {
		if _, err := existingTask(k.ctx, conn, tid); err != nil {
			return "", err
		}
		stored, err := ListAttachments(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		attachments := make([]any, 0, len(stored))
		for _, a := range stored {
			attachments = append(attachments, fieldsOf(a, attachmentFields))
		}
		return pyjson.Dumps(pyjson.ObjectOf("ok", true, "task_id", tid, "attachments", attachments))
	})
}
