package gateway

import (
	"fmt"
	"net/http"
	"os"
)

// Wire writer that records the status the file server answered with
type webStatusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *webStatusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// File body with ETag, Last-Modified, conditional and range answers like aiohttp FileResponse
func WebFileResponse(request *WebRequest, path string, headers map[string]string) (*WebResponse, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	resp := NewWebResponse(http.StatusOK, headers)
	resp.Headers.Set("ETag", fmt.Sprintf("\"%x-%x\"", st.ModTime().UnixNano(), st.Size()))
	if request.writer == nil {
		body := make([]byte, st.Size())
		n, err := f.ReadAt(body, 0)
		if err != nil && n < len(body) {
			return nil, err
		}
		resp.Body = body[:n]
		resp.Headers.Set("Last-Modified", st.ModTime().UTC().Format(http.TimeFormat))
		resp.Headers.Set("Accept-Ranges", "bytes")
		return resp, nil
	}
	rec := &webStatusRecorder{ResponseWriter: request.writer, status: http.StatusOK}
	for k, values := range resp.Headers {
		for _, v := range values {
			rec.Header().Add(k, v)
		}
	}
	wireReq := &http.Request{Method: request.Method, Header: request.Header, URL: request.RelURL}
	http.ServeContent(rec, wireReq, st.Name(), st.ModTime(), f)
	resp.prepared = true
	resp.Status = rec.status
	resp.streamed = int(st.Size())
	return resp, nil
}
