package web

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	firecrawlconfig "github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/firecrawl/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/web"
)

// Longest Firecrawl Parse may take to read a file
const parseTimeout = 300 * time.Second

// Characters a file name in a form part header cannot hold
var unquotable = strings.NewReplacer(`"`, "_", "\r", "_", "\n", "_")

// Markdown Firecrawl Parse reads from a whole PDF, scanned pages included, ending in a newline
func parse(ctx context.Context, cfg *config.Config, data []byte, filename string) (string, error) {
	body, contentType, err := form(data, filename)
	if err != nil {
		return "", errors.New("Firecrawl Parse: " + err.Error())
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, cmp.Or(address(cfg), cloud)+"/v2/parse", bytes.NewReader(body))
	if err != nil {
		return "", errors.New("Firecrawl Parse: " + err.Error())
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("User-Agent", provider.UserAgent("icebath"))
	secret := key(cfg)
	if secret != "" {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	response, err := (&http.Client{Timeout: parseTimeout}).Do(request)
	if err != nil {
		return "", errors.New("Firecrawl Parse: " + err.Error())
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return "", errors.New("Firecrawl Parse: " + err.Error())
	}
	var reply struct {
		Success bool     `json:"success"`
		Error   web.Text `json:"error"`
		Data    struct {
			Markdown any `json:"markdown"`
		} `json:"data"`
	}
	// A reply that is no JSON object, or no valid UTF-8, says nothing beyond its status
	if utf8.Valid(raw) {
		_ = json.Unmarshal(bytes.TrimPrefix(raw, []byte("\uFEFF")), &reply)
	}
	if response.StatusCode != http.StatusOK || !reply.Success {
		return "", errors.New(refusal(response.StatusCode, cmp.Or(reply.Error.String(), fmt.Sprintf("HTTP %d", response.StatusCode)), secret != ""))
	}
	markdown, _ := reply.Data.Markdown.(string)
	if markdown == "" {
		return "", errors.New("Firecrawl Parse returned no Markdown")
	}
	if !strings.HasSuffix(markdown, "\n") {
		markdown += "\n"
	}
	return markdown, nil
}

// Multipart body of the parse options and the file, and its content type
func form(data []byte, filename string) ([]byte, string, error) {
	options, err := json.Marshal(map[string]any{
		"parsers": []map[string]string{{"type": "pdf", "mode": "auto"}},
		"origin":  "icebath@" + provider.Version(),
	})
	if err != nil {
		return nil, "", err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	parts := []struct {
		header  textproto.MIMEHeader
		content []byte
	}{
		{textproto.MIMEHeader{"Content-Disposition": {`form-data; name="options"`}}, options},
		{textproto.MIMEHeader{
			"Content-Disposition": {`form-data; name="file"; filename="` + unquotable.Replace(filename) + `"`},
			"Content-Type":        {"application/pdf"},
		}, data},
	}
	for _, part := range parts {
		w, err := writer.CreatePart(part.header)
		if err != nil {
			return nil, "", err
		}
		if _, err := w.Write(part.content); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

// Why Firecrawl Parse refused the file, from its status, what it said and whether a key was sent
func refusal(status int, detail string, keyed bool) string {
	switch {
	case status == http.StatusUnauthorized:
		return "Firecrawl Parse rejected the API key: " + detail
	case status == http.StatusPaymentRequired:
		return "Firecrawl Parse is out of credits: " + detail
	case status == http.StatusTooManyRequests && keyed:
		return "Firecrawl Parse rate limit reached: " + detail
	case status == http.StatusTooManyRequests:
		return "Firecrawl Parse keyless limit reached, set " + firecrawlconfig.APIKeyEnv + ": " + detail
	}
	return "Firecrawl Parse: " + detail
}
