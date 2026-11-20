package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"strings"
)

// One form field of an httpx multipart body, a file part when Filename is set
type MultipartPart struct {
	Name        string
	Filename    string
	ContentType string
	Data        []byte
}

// Header parameter quoted the way httpx encodes HTML5 form names and filenames
func httpxFormParam(name, value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r == '"':
			b.WriteString("%22")
		case r == '\\':
			b.WriteString(`\\`)
		case r <= 0x1F && r != 0x1B:
			fmt.Fprintf(&b, "%%%02X", r)
		default:
			b.WriteRune(r)
		}
	}
	return name + `="` + b.String() + `"`
}

// Multipart body and content type of the parts in order under a random hex boundary like httpx
func EncodeHTTPXMultipart(parts []MultipartPart) ([]byte, string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.SetBoundary(hex.EncodeToString(raw)); err != nil {
		return nil, "", err
	}
	for _, p := range parts {
		header := textproto.MIMEHeader{}
		disposition := "form-data; " + httpxFormParam("name", p.Name)
		if p.Filename != "" {
			disposition += "; " + httpxFormParam("filename", p.Filename)
		}
		if p.ContentType != "" {
			header.Set("Content-Type", p.ContentType)
		}
		header.Set("Content-Disposition", disposition)
		pw, err := w.CreatePart(header)
		if err != nil {
			return nil, "", err
		}
		if _, err := pw.Write(p.Data); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}
