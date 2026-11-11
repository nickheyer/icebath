package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pymultipart"
	"github.com/nickheyer/icebath/pkg/internal/pytempfile"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Readable, writable and seekable file an upload holds
type UploadFileIO interface {
	io.Reader
	io.Writer
	io.Seeker
	io.Closer
}

// Uploaded file of a form, the Starlette UploadFile
type UploadFile struct {
	Filename *string
	File     UploadFileIO
	Size     *int64
	Headers  Headers
}

func (*UploadFile) PyTypeName() string { return "UploadFile" }

// Content type header of the part, false when absent
func (u *UploadFile) ContentType() (string, bool) { return u.Headers.Lookup("content-type") }

// Appends data at the file position, counting it into the size when one is kept
func (u *UploadFile) Write(data []byte) error {
	if u.Size != nil {
		*u.Size += int64(len(data))
	}
	_, err := u.File.Write(data)
	return err
}

// Up to size bytes from the position, everything left for a negative size
func (u *UploadFile) Read(size int) ([]byte, error) {
	if size < 0 {
		return io.ReadAll(u.File)
	}
	buf := make([]byte, size)
	n, err := io.ReadFull(u.File, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		err = nil
	}
	return buf[:n], err
}

// Moves the position to an absolute offset
func (u *UploadFile) SeekTo(offset int64) error {
	_, err := u.File.Seek(offset, io.SeekStart)
	return err
}

// Closes the underlying file
func (u *UploadFile) Close() error { return u.File.Close() }

// One key and value of a form, the value a string or an upload
type FormItem struct {
	Key   string
	Value any
}

// Immutable multidict of text fields and uploads, the Starlette FormData
type FormData struct {
	items []FormItem
}

// Form over its items in order
func NewFormData(items []FormItem) *FormData { return &FormData{items: items} }

// Last value of a key and whether the key is present
func (f *FormData) Get(key string) (any, bool) {
	var v any
	found := false
	for _, it := range f.items {
		if it.Key == key {
			v, found = it.Value, true
		}
	}
	return v, found
}

// Every value of a key in order
func (f *FormData) GetList(key string) []any {
	var out []any
	for _, it := range f.items {
		if it.Key == key {
			out = append(out, it.Value)
		}
	}
	return out
}

// Distinct keys in first seen order
func (f *FormData) Keys() []string {
	var keys []string
	seen := map[string]bool{}
	for _, it := range f.items {
		if !seen[it.Key] {
			seen[it.Key] = true
			keys = append(keys, it.Key)
		}
	}
	return keys
}

// Items in order including repeated keys
func (f *FormData) MultiItems() []FormItem { return f.items }

// Closes every upload of the form
func (f *FormData) Close() error {
	var first error
	for _, it := range f.items {
		if u, ok := it.Value.(*UploadFile); ok {
			if err := u.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// Text of bytes in a codec, latin-1 when the codec is unknown or the bytes do not decode
func userSafeDecode(src []byte, codec string) string {
	if s, err := pycodecs.Decode(src, codec); err == nil {
		return s
	}
	return latin1Decode(string(src))
}

// Python urllib.parse.unquote_plus of text
func unquotePlus(s string) string { return pyurl.Unquote(strings.ReplaceAll(s, "+", " ")) }

// Body stream feeding each chunk to yield, ending with an empty chunk
type bodyStream func(yield func([]byte) error) error

const (
	formFieldStart = iota + 1
	formFieldName
	formFieldData
	formFieldEnd
	formEnd
)

type formMessage struct {
	kind int
	data []byte
}

// Urlencoded form of a body stream, field size and count limits raised as MultiPartException
func parseURLEncodedForm(stream bodyStream, maxFields, maxPartSize int) (*FormData, error) {
	var messages []formMessage
	currentFieldSize, currentFields := 0, 0
	sizeCheck := func(n int) error {
		currentFieldSize += n
		if currentFieldSize > maxPartSize {
			return &MultiPartException{Message: fmt.Sprintf("Field exceeded maximum size of %dKB.", maxPartSize/1024)}
		}
		return nil
	}
	parser, err := pymultipart.NewQuerystringParser(pymultipart.QuerystringCallbacks{
		OnFieldStart: func() error {
			currentFieldSize = 0
			messages = append(messages, formMessage{formFieldStart, nil})
			return nil
		},
		OnFieldName: func(data []byte) error {
			if err := sizeCheck(len(data)); err != nil {
				return err
			}
			messages = append(messages, formMessage{formFieldName, append([]byte{}, data...)})
			return nil
		},
		OnFieldData: func(data []byte) error {
			if err := sizeCheck(len(data)); err != nil {
				return err
			}
			messages = append(messages, formMessage{formFieldData, append([]byte{}, data...)})
			return nil
		},
		OnFieldEnd: func() error {
			currentFields++
			if currentFields > maxFields {
				return &MultiPartException{Message: fmt.Sprintf("Too many fields. Maximum number of fields is %d.", maxFields)}
			}
			messages = append(messages, formMessage{formFieldEnd, nil})
			return nil
		},
		OnEnd: func() error {
			messages = append(messages, formMessage{formEnd, nil})
			return nil
		},
	}, false, math.Inf(1))
	if err != nil {
		return nil, err
	}
	var fieldName, fieldValue []byte
	var items []FormItem
	err = stream(func(chunk []byte) error {
		if len(chunk) > 0 {
			if _, err := parser.Write(chunk); err != nil {
				return err
			}
		} else if err := parser.Finalize(); err != nil {
			return err
		}
		pending := messages
		messages = nil
		for _, m := range pending {
			switch m.kind {
			case formFieldStart:
				fieldName, fieldValue = nil, nil
			case formFieldName:
				fieldName = append(fieldName, m.data...)
			case formFieldData:
				fieldValue = append(fieldValue, m.data...)
			case formFieldEnd:
				items = append(items, FormItem{unquotePlus(latin1Decode(string(fieldName))), unquotePlus(latin1Decode(string(fieldValue)))})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return NewFormData(items), nil
}

// One part of a multipart body while it is parsed
type multipartPart struct {
	contentDisposition *string
	fieldName          string
	data               []byte
	file               *UploadFile
	itemHeaders        []Header
}

type filePartWrite struct {
	part *multipartPart
	data []byte
}

const (
	multipartSpoolMaxSize = 1024 * 1024
)

// ASCII lowercase of raw bytes, the Python bytes.lower
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// Multipart form of a body stream with uploads spooled to temporary files, limits raised as MultiPartException
func parseMultipartForm(headers Headers, stream bodyStream, maxFiles, maxFields, maxPartSize int) (*FormData, error) {
	var items []FormItem
	currentFiles, currentFields := 0, 0
	var partialHeaderName, partialHeaderValue string
	current := &multipartPart{}
	var fileParts []filePartWrite
	var filesToFinish []*multipartPart
	var filesToCloseOnError []*pytempfile.SpooledTemporaryFile

	_, params := pymultipart.ParseOptionsHeader(headers.Get("Content-Type"))
	charset := "utf-8"
	if cs, ok := params["charset"]; ok {
		charset = latin1Decode(cs)
	}
	if mod, ok := pycodecs.Lookup(charset); ok {
		charset = mod
	} else {
		charset = "latin-1"
	}
	boundary, ok := params["boundary"]
	if !ok {
		return nil, &MultiPartException{Message: "Missing boundary in multipart."}
	}
	callbacks := pymultipart.MultipartCallbacks{
		OnPartBegin: func() error {
			current = &multipartPart{}
			return nil
		},
		OnPartData: func(data []byte) error {
			if current.file == nil {
				if len(current.data)+len(data) > maxPartSize {
					return &MultiPartException{Message: fmt.Sprintf("Part exceeded maximum size of %dKB.", maxPartSize/1024)}
				}
				current.data = append(current.data, data...)
				return nil
			}
			fileParts = append(fileParts, filePartWrite{current, append([]byte{}, data...)})
			return nil
		},
		OnPartEnd: func() error {
			if current.file == nil {
				items = append(items, FormItem{current.fieldName, userSafeDecode(current.data, charset)})
				return nil
			}
			filesToFinish = append(filesToFinish, current)
			items = append(items, FormItem{current.fieldName, current.file})
			return nil
		},
		OnHeaderField: func(data []byte) error {
			partialHeaderName += string(data)
			return nil
		},
		OnHeaderValue: func(data []byte) error {
			partialHeaderValue += string(data)
			return nil
		},
		OnHeaderEnd: func() error {
			field := asciiLower(partialHeaderName)
			if field == "content-disposition" {
				v := partialHeaderValue
				current.contentDisposition = &v
			}
			current.itemHeaders = append(current.itemHeaders, Header{field, partialHeaderValue})
			partialHeaderName, partialHeaderValue = "", ""
			return nil
		},
		OnHeadersFinished: func() error {
			disposition := ""
			if current.contentDisposition != nil {
				disposition = latin1Decode(*current.contentDisposition)
			}
			_, options := pymultipart.ParseOptionsHeader(disposition)
			name, ok := options["name"]
			if !ok {
				return &MultiPartException{Message: `The Content-Disposition header field "name" must be provided.`}
			}
			current.fieldName = userSafeDecode([]byte(name), charset)
			if rawFilename, ok := options["filename"]; ok {
				currentFiles++
				if currentFiles > maxFiles {
					return &MultiPartException{Message: fmt.Sprintf("Too many files. Maximum number of files is %d.", maxFiles)}
				}
				filename := userSafeDecode([]byte(rawFilename), charset)
				tempfile := pytempfile.NewSpooledTemporaryFile(multipartSpoolMaxSize)
				filesToCloseOnError = append(filesToCloseOnError, tempfile)
				size := int64(0)
				part := current
				current.file = &UploadFile{Filename: &filename, File: tempfile, Size: &size, Headers: headersOfRaw(&part.itemHeaders)}
				return nil
			}
			currentFields++
			if currentFields > maxFields {
				return &MultiPartException{Message: fmt.Sprintf("Too many fields. Maximum number of fields is %d.", maxFields)}
			}
			current.file = nil
			return nil
		},
		OnEnd: func() error { return nil },
	}
	err := func() error {
		parser, err := pymultipart.NewMultipartParser([]byte(boundary), callbacks, math.Inf(1),
			pymultipart.DefaultMaxHeaderCount, pymultipart.DefaultMaxHeaderSize)
		if err != nil {
			return err
		}
		if err := stream(func(chunk []byte) error {
			if _, err := parser.Write(chunk); err != nil {
				return err
			}
			for _, fp := range fileParts {
				if err := fp.part.file.Write(fp.data); err != nil {
					return err
				}
			}
			for _, part := range filesToFinish {
				if err := part.file.SeekTo(0); err != nil {
					return err
				}
			}
			fileParts, filesToFinish = nil, nil
			return nil
		}); err != nil {
			return err
		}
		return parser.Finalize()
	}()
	if err != nil {
		for _, f := range filesToCloseOnError {
			f.Close()
		}
		var fpe *pymultipart.FormParserError
		if errors.As(err, &fpe) {
			return nil, &MultiPartException{Message: "Invalid multipart data."}
		}
		return nil, err
	}
	return NewFormData(items), nil
}

// Parsed form of a multipart or urlencoded body, cached on the request, an empty form for any other content type
func (r *Request) Form(ctx context.Context, maxFiles, maxFields, maxPartSize int) (*FormData, error) {
	if r.form == nil {
		contentType, _ := pymultipart.ParseOptionsHeader(r.Headers().Get("Content-Type"))
		stream := func(yield func([]byte) error) error { return r.Stream(ctx, yield) }
		var form *FormData
		var err error
		switch contentType {
		case "multipart/form-data":
			form, err = parseMultipartForm(r.Headers(), stream, maxFiles, maxFields, maxPartSize)
		case "application/x-www-form-urlencoded":
			form, err = parseURLEncodedForm(stream, maxFields, maxPartSize)
		default:
			form = NewFormData(nil)
		}
		if err != nil {
			var mpe *MultiPartException
			if errors.As(err, &mpe) && r.Scope.App != nil {
				return nil, NewHTTPException(400, mpe.Message)
			}
			return nil, err
		}
		r.form = form
	}
	return r.form, nil
}

// Closes the uploads of a parsed form
func (r *Request) Close() error {
	if r.form != nil {
		return r.form.Close()
	}
	return nil
}
