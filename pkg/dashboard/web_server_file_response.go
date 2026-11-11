package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
)

// File response that serves the descriptor its opener returns, never reopening the path
func NewOpenedFileResponse(path string, status int, mediaType string, filename *string, dispositionType string,
	opener func() (*os.File, error), headers ...string) *FileResponse {
	r := NewFileResponse(path, status, mediaType, filename, dispositionType, headers...)
	r.opener = opener
	return r
}

// Sends the pinned file bytes from start up to end, stopping early at a short file
func (r *FileResponse) sendRange(ctx context.Context, send Send, start, end int64) error {
	if _, err := r.pinned.Seek(start, io.SeekStart); err != nil {
		return err
	}
	for start < end {
		chunk := make([]byte, min(int64(r.chunkSize), end-start))
		n, err := r.pinned.Read(chunk)
		if n == 0 {
			if err != nil && err != io.EOF {
				return err
			}
			return nil
		}
		start += int64(n)
		if err := send(ctx, Message{Type: "http.response.body", Body: chunk[:n], MoreBody: true}); err != nil {
			return err
		}
	}
	return nil
}

func (r *FileResponse) pinnedSimple(ctx context.Context, send Send, headerOnly bool) error {
	if err := send(ctx, Message{Type: "http.response.start", Status: r.StatusCode, Headers: r.RawHeaders}); err != nil {
		return err
	}
	if !headerOnly {
		if err := r.sendRange(ctx, send, 0, r.stat.Size()); err != nil {
			return err
		}
	}
	return send(ctx, Message{Type: "http.response.body", Body: []byte{}})
}

func (r *FileResponse) pinnedSingleRange(ctx context.Context, send Send, start, end, size int64, headerOnly bool) error {
	raw := slices.Clone(r.RawHeaders)
	h := headersOfRaw(&raw)
	h.Set("content-range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, size))
	h.Set("content-length", strconv.FormatInt(end-start, 10))
	if err := send(ctx, Message{Type: "http.response.start", Status: 206, Headers: raw}); err != nil {
		return err
	}
	if !headerOnly {
		if err := r.sendRange(ctx, send, start, end); err != nil {
			return err
		}
	}
	return send(ctx, Message{Type: "http.response.body", Body: []byte{}})
}

func (r *FileResponse) pinnedMultipleRanges(ctx context.Context, send Send, ranges [][2]int64, size int64, headerOnly bool) error {
	boundaryBytes := make([]byte, 13)
	rand.Read(boundaryBytes)
	boundary := hex.EncodeToString(boundaryBytes)
	contentType := r.Header().Get("content-type")
	static := int64(49 + len(boundary) + len(contentType) + len(strconv.FormatInt(size, 10)))
	length := int64(4 + len(boundary))
	for _, rg := range ranges {
		length += int64(len(strconv.FormatInt(rg[0], 10))+len(strconv.FormatInt(rg[1]-1, 10))) + static + rg[1] - rg[0]
	}
	raw := slices.Clone(r.RawHeaders)
	h := headersOfRaw(&raw)
	h.Set("content-type", "multipart/byteranges; boundary="+boundary)
	h.Set("content-length", strconv.FormatInt(length, 10))
	if err := send(ctx, Message{Type: "http.response.start", Status: 206, Headers: raw}); err != nil {
		return err
	}
	if !headerOnly {
		for _, rg := range ranges {
			part := fmt.Sprintf("--%s\r\nContent-Type: %s\r\nContent-Range: bytes %d-%d/%d\r\n\r\n", boundary, contentType, rg[0], rg[1]-1, size)
			if err := send(ctx, Message{Type: "http.response.body", Body: []byte(mustLatin1(part)), MoreBody: true}); err != nil {
				return err
			}
			if err := r.sendRange(ctx, send, rg[0], rg[1]); err != nil {
				return err
			}
			if err := send(ctx, Message{Type: "http.response.body", Body: []byte("\r\n"), MoreBody: true}); err != nil {
				return err
			}
		}
		if err := send(ctx, Message{Type: "http.response.body", Body: []byte("--" + boundary + "--"), MoreBody: true}); err != nil {
			return err
		}
	}
	return send(ctx, Message{Type: "http.response.body", Body: []byte{}})
}
