package gateway

import (
	"errors"
	"net/http"
)

// WebStreamResponse is an aiohttp StreamResponse, its status and headers sent at prepare and its body written in parts
type WebStreamResponse struct {
	*WebResponse
	writer http.ResponseWriter
}

// Stream response with the status and headers, sent to the client once prepared
func NewWebStreamResponse(status int, headers map[string]string) *WebStreamResponse {
	return &WebStreamResponse{WebResponse: NewWebResponse(status, headers)}
}

// Sends the status and headers to the request's client, later header changes having no effect
func (s *WebStreamResponse) Prepare(request *WebRequest) error {
	if s.prepared {
		return nil
	}
	s.prepared = true
	s.writer = request.writer
	if s.writer == nil {
		return nil
	}
	for k, values := range s.Headers {
		for _, v := range values {
			s.writer.Header().Add(k, v)
		}
	}
	s.writer.WriteHeader(s.Status)
	if flusher, ok := s.writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// Writes one body part and flushes it, kept in Body when no client is attached, a closed client as ConnectionResetError
func (s *WebStreamResponse) Write(data []byte) error {
	if !s.prepared {
		return errors.New("RuntimeError: Cannot call write() for not prepared StreamResponse")
	}
	if s.writer == nil {
		s.Body = append(s.Body, data...)
		return nil
	}
	s.streamed += len(data)
	if _, err := s.writer.Write(data); err != nil {
		return &PyError{Kind: "ConnectionResetError", Msg: "Cannot write to closing transport"}
	}
	if flusher, ok := s.writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// Ends the body, nothing further may be written
func (s *WebStreamResponse) WriteEOF() error { return nil }
