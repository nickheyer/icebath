package acputil

import jsonutil "github.com/nickheyer/icebath/pkg/utils/json"

// Params of fs/read_text_file: the file and, optionally, the line to start at and how many lines to read
type ReadTextFileRequest struct {
	Meta      *jsonutil.Object `json:"_meta,omitempty"`
	Limit     *int             `json:"limit,omitempty"`
	Line      *int             `json:"line,omitempty"`
	Path      string           `json:"path"`
	SessionID string           `json:"sessionId"`
}

// Fills the request from its JSON object, which must carry path and sessionId
func (r *ReadTextFileRequest) UnmarshalJSON(data []byte) error {
	type plain ReadTextFileRequest
	return decodeObject(data, (*plain)(r), "ReadTextFileRequest", "path", "sessionId")
}

// Result of fs/read_text_file
type ReadTextFileResponse struct {
	Meta    *jsonutil.Object `json:"_meta,omitempty"`
	Content string           `json:"content"`
}

// Fills the response from its JSON object, which must carry content
func (r *ReadTextFileResponse) UnmarshalJSON(data []byte) error {
	type plain ReadTextFileResponse
	return decodeObject(data, (*plain)(r), "ReadTextFileResponse", "content")
}

// Params of fs/write_text_file
type WriteTextFileRequest struct {
	Meta      *jsonutil.Object `json:"_meta,omitempty"`
	Content   string           `json:"content"`
	Path      string           `json:"path"`
	SessionID string           `json:"sessionId"`
}

// Fills the request from its JSON object, which must carry content, path and sessionId
func (r *WriteTextFileRequest) UnmarshalJSON(data []byte) error {
	type plain WriteTextFileRequest
	return decodeObject(data, (*plain)(r), "WriteTextFileRequest", "content", "path", "sessionId")
}

// Result of fs/write_text_file
type WriteTextFileResponse struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
}
