package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/desktoplog"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
)

// Directory under ICEBATH_HOME the backend admits pasted text attachments from
const ComposerPastesDirname = "composer-pastes"

var (
	composerExt       = regexp.MustCompile(`^\.[a-z0-9]{1,5}$`)
	composerNameSplit = regexp.MustCompile(`[\\/]`)
	composerLastExt   = regexp.MustCompile(`\.[^.]+$`)
	composerUnsafe    = regexp.MustCompile(`[^\p{L}\p{N}._-]+`)
	composerEdges     = regexp.MustCompile(`^[._-]+|[._-]+$`)
)

// Local wall clock stamp of a pasted image name, YYYY-MM-DD_HH-MM-SS-mmm
func ComposerImageTimestamp(t time.Time) string {
	return fmt.Sprintf("%04d-%02d-%02d_%02d-%02d-%02d-%03d", t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond()/1e6)
}

// Writes a large plain text paste as a generated txt file under the composer pastes directory
func WriteComposerPaste(hermesHome, text string, now time.Time) (string, error) {
	dir := filepath.Join(hermesHome, ComposerPastesDirname)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", hardening.NodeErr(err)
	}
	u := now.UTC()
	stamp := fmt.Sprintf("%04d-%02d-%02d_%02d-%02d-%02d-%03d", u.Year(), int(u.Month()), u.Day(), u.Hour(), u.Minute(), u.Second(), u.Nanosecond()/1e6)
	file := filepath.Join(dir, "pasted_content_"+stamp+"_"+randomHex(3)+".txt")
	if err := os.WriteFile(file, []byte(text), 0o666); err != nil {
		return "", hardening.NodeErr(err)
	}
	return file, nil
}

// Writes pasted image bytes under composer-images with a sanitized name or a local time stamp
func WriteComposerImage(userData string, data []byte, ext, name string, now time.Time) (string, error) {
	rawExt := strings.ToLower(jsTrim(ext))
	normalized := rawExt
	if !strings.HasPrefix(rawExt, ".") {
		normalized = "." + rawExt
	}
	safeExt := ".png"
	if composerExt.MatchString(normalized) {
		safeExt = normalized
	}
	dir := filepath.Join(userData, "composer-images")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", hardening.NodeErr(err)
	}
	stamp := ComposerImageTimestamp(now)
	random := randomHex(3)
	parts := composerNameSplit.Split(name, -1)
	base := composerLastExt.ReplaceAllString(parts[len(parts)-1], "")
	safeName := composerEdges.ReplaceAllString(composerUnsafe.ReplaceAllString(base, "_"), "")
	safeName = desktoplog.JSSlice(safeName, 80)
	fileName := "composer_" + stamp + "_" + random + safeExt
	if safeName != "" {
		fileName = safeName + "_" + random + safeExt
	}
	file := filepath.Join(dir, fileName)
	if err := os.WriteFile(file, data, 0o666); err != nil {
		return "", hardening.NodeErr(err)
	}
	return file, nil
}

// JavaScript truthiness of a JSON value
func jsTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0 && !math.IsNaN(t)
	case string:
		return t != ""
	}
	return true
}

// Answers saveImageBuffer, data nil when the renderer sent none
func (s *Service) SaveImageBuffer(data []byte, ext, name json.RawMessage) (string, error) {
	if data == nil {
		return "", errors.New("saveImageBuffer: missing data")
	}
	extValue, _ := jsParse(string(ext))
	extText := ".png"
	if jsTruthy(extValue) {
		extText = jsString(extValue)
	}
	nameValue, _ := jsParse(string(name))
	nameText := ""
	if jsTruthy(nameValue) {
		nameText = jsString(nameValue)
	}
	return WriteComposerImage(s.userData, data, extText, nameText, time.Now())
}

// Answers savePastedText with the path of the written paste file
func (s *Service) SavePastedText(text json.RawMessage) (string, error) {
	v, _ := jsParse(string(text))
	t, _ := v.(string)
	if t == "" {
		return "", errors.New("savePastedText: missing text")
	}
	return WriteComposerPaste(s.hermesHome, t, time.Now())
}
