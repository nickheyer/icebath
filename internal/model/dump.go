package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/wire"
)

func (s *Session) DumpRequest(ctx context.Context, p *Prepared, reason string, cause error) (string, error) {
	if p == nil || p.Request == nil {
		return "", errors.New("model: a request dump needs a prepared request")
	}
	ctx = s.r.bind(ctx)
	call := p.Sent
	if call == nil {
		call = p.Call
	}
	var body []byte
	if call != nil {
		body = call.Body
	}
	stdout, err := s.envEnabled(ctx, "ICEBATH_DUMP_REQUEST_STDOUT")
	if err != nil {
		return "", err
	}
	return s.writeDump(ctx, s.Route(), p.Request.SessionID, body, reason, cause, stdout)
}

func (s *Session) dump(ctx context.Context, x *exchange, reason string, cause error) (string, error) {
	return s.writeDump(ctx, x.rt, x.p.Request.SessionID, x.call.Body, reason, cause, x.k.dumpStdout)
}

func safeSessionComponent(sessionID string) string {
	raw := strings.TrimSpace(sessionID)
	var b strings.Builder
	for _, r := range raw {
		if r == '_' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	sanitized := strings.Trim(b.String(), "._")
	if runes := []rune(sanitized); len(runes) > 96 {
		sanitized = string(runes[:96])
	}
	if sanitized == "" {
		sanitized = "session"
	}
	if raw != "" && sanitized == raw {
		return sanitized
	}
	sum := sha256.Sum256([]byte(raw))
	return sanitized + "_" + hex.EncodeToString(sum[:])[:12]
}

func isoTimestamp(t time.Time) string {
	if t.Nanosecond()/1000 == 0 {
		return t.Format("2006-01-02T15:04:05")
	}
	return t.Format("2006-01-02T15:04:05.000000")
}

func dumpEndpoint(mode wire.Mode) string {
	switch mode {
	case wire.ModeResponses:
		return "/responses"
	case wire.ModeAnthropic:
		return "/messages"
	}
	return "/chat/completions"
}

func errorDebugInfo(cause error) json.RawMessage {
	d := (&wire.Dict{}).SetString("type", errorTypeName(cause)).SetString("message", cause.Error())
	var ae *wire.APIError
	if errors.As(cause, &ae) {
		if ae.Status != 0 {
			d.SetInt("status_code", ae.Status)
		}
		if id := ae.Header.Get("x-request-id"); id != "" {
			d.SetString("request_id", id)
		}
		if code := ae.Code(); code != "" {
			d.SetString("code", code)
		}
		body := ae.Body()
		if v, err := wire.ParseValue(body); err == nil {
			if t := v.Path("error", "type").Text(); t != "" {
				d.SetString("type", t)
			}
			if param := v.Path("error", "param"); param.Truthy() {
				d.Set("param", param.Raw())
			}
			d.Set("body", body)
		}
		if ae.Status != 0 {
			d.SetInt("response_status", ae.Status)
			d.SetString("response_text", string(body))
		}
	}
	return d.JSON()
}

func (s *Session) writeDump(ctx context.Context, rt *Route, sessionID string, body []byte, reason string, cause error, stdout bool) (string, error) {
	now := time.Now()
	bodyDict := &wire.Dict{}
	if v, err := wire.ParseValue(body); err == nil && v.IsObject() {
		for _, f := range v.Fields() {
			if f.Value.Kind() == wire.KindNull || f.Key == "timeout" {
				continue
			}
			bodyDict.Set(f.Key, f.Value.Raw())
		}
	}
	masked := MaskCredential(rt.Credential)
	if masked == "" {
		masked = "None"
	}
	headers := (&wire.Dict{}).SetString("Authorization", "Bearer "+masked).SetString("Content-Type", "application/json")
	request := (&wire.Dict{}).SetString("method", "POST").SetString("url", strings.TrimRight(rt.BaseURL, "/")+dumpEndpoint(rt.Mode)).
		Set("headers", headers.JSON()).Set("body", bodyDict.JSON())
	payload := (&wire.Dict{}).SetString("timestamp", isoTimestamp(now))
	if sessionID == "" {
		payload.Set("session_id", json.RawMessage("null"))
	} else {
		payload.SetString("session_id", sessionID)
	}
	payload.SetString("reason", reason).Set("request", request.JSON())
	if cause != nil {
		payload.Set("error", errorDebugInfo(cause))
	}
	text, ok := wire.FormatJSON(payload.JSON(), wire.TextLayout{Spaced: true, UTF8: true})
	if !ok {
		return "", errors.New("model: request dump payload is not valid JSON")
	}
	redacted := wire.MaskCredentials(text)
	if !json.Valid([]byte(redacted)) {
		return "", errors.New("model: request dump is not valid JSON after redaction")
	}
	dir := home.Logs.In(s.r.c.o.Home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("request_dump_%s_%s_%06d.json", safeSessionComponent(sessionID), now.Format("20060102_150405"), now.Nanosecond()/1000))
	if err := fileutil.WriteJSON(ctx, path, json.RawMessage(redacted), fileutil.JSONStyle{Indent: 2}); err != nil {
		return "", err
	}
	slog.Info("Request debug dump written to: " + path)
	if stdout {
		out, err := fileutil.EncodeJSON(json.RawMessage(redacted), fileutil.JSONStyle{Indent: 2})
		if err != nil {
			return path, err
		}
		if _, err := s.r.c.o.DumpOutput.Write(append(out, '\n')); err != nil {
			return path, err
		}
	}
	return path, nil
}
