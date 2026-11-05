package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// ASCII only identifier safe inside a mapping file name, a trailing newline allowed as Python's dollar does
var safeWhatsAppIdentifierRE = regexp.MustCompile(`\A[A-Za-z0-9@.+\-]+\n?\z`)

// Bare numeric identifier with JID, LID, device and plus syntax stripped
func NormalizeWhatsAppIdentifier(value any) string {
	s := ""
	if pyrepr.Truthy(value) {
		s = pyrepr.Str(value)
	}
	s = strings.Replace(pystr.Strip(s), "+", "", 1)
	s, _, _ = strings.Cut(s, ":")
	s, _, _ = strings.Cut(s, "@")
	return s
}

// Optional plus then Unicode digits, whitespace, parentheses, dots and dashes
func isBarePhone(s string) bool {
	s = strings.TrimPrefix(s, "+")
	if s == "" {
		return false
	}
	for _, r := range s {
		if !pystr.IsDecimal(r) && !pystr.IsSpace(r) && !strings.ContainsRune("().-", r) {
			return false
		}
	}
	return true
}

// Bridge safe outbound JID, a bare phone gaining the user server and a device suffix dropped
func ToWhatsAppJID(value string) string {
	if value == "" {
		return ""
	}
	normalized := pystr.Strip(value)
	if strings.Contains(normalized, ":") && strings.Contains(normalized, "@") {
		prefix, domain, _ := strings.Cut(normalized, "@")
		user, _, _ := strings.Cut(prefix, ":")
		normalized = user + "@" + domain
	}
	if strings.Contains(normalized, "@") {
		return normalized
	}
	if isBarePhone(normalized) {
		var digits strings.Builder
		for _, r := range normalized {
			if pystr.IsDecimal(r) {
				digits.WriteRune(r)
			}
		}
		if digits.Len() > 0 {
			return digits.String() + "@s.whatsapp.net"
		}
	}
	return normalized
}

// Participant JID valid for an outbound mention, empty otherwise
func NormalizeWhatsAppMentionJID(value string) string {
	jid := ToWhatsAppJID(value)
	user, domain, found := strings.Cut(jid, "@")
	if !found || user == "" || (domain != "s.whatsapp.net" && domain != "lid") {
		return ""
	}
	for i := 0; i < len(user); i++ {
		if user[i] < '0' || user[i] > '9' {
			return ""
		}
	}
	return jid
}

// Every identifier reachable through the bridge lid mapping files, the normalized input included
func expandWhatsAppAliases(ctx context.Context, identifier string) (map[string]bool, error) {
	resolved := map[string]bool{}
	normalized := NormalizeWhatsAppIdentifier(identifier)
	if normalized == "" {
		return resolved, nil
	}
	sessionDir := home.HermesDir(home.Dir(ctx), "platforms/whatsapp/session", "whatsapp/session")
	queue := []string{normalized}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current == "" || resolved[current] || !safeWhatsAppIdentifierRE.MatchString(current) {
			continue
		}
		resolved[current] = true
		for _, suffix := range []string{"", "_reverse"} {
			mappingPath := filepath.Join(sessionDir, "lid-mapping-"+current+suffix+".json")
			if exists, err := pathExists(mappingPath); err != nil {
				return nil, err
			} else if !exists {
				continue
			}
			mapped, err := readLIDMapping(mappingPath)
			if err != nil {
				var decodeErr *pyjson.DecodeError
				if errors.As(err, &decodeErr) || errors.As(err, new(*fs.PathError)) {
					slog.Debug(fmt.Sprintf("whatsapp_identity: failed to read %s: %s", mappingPath, err))
					continue
				}
				return nil, err
			}
			if mapped != "" && !resolved[mapped] {
				queue = append(queue, mapped)
			}
		}
	}
	return resolved, nil
}

// Path.exists of Python 3.11, false for a missing path, a loop or a non directory parent and an error otherwise
func pathExists(p string) (bool, error) {
	_, err := os.Stat(p)
	if err == nil {
		return true, nil
	}
	for _, errno := range []syscall.Errno{syscall.ENOENT, syscall.ENOTDIR, syscall.EBADF, syscall.ELOOP} {
		if errors.Is(err, errno) {
			return false, nil
		}
	}
	return false, err
}

// Normalized identifier a lid mapping file holds, read as utf-8-sig JSON
func readLIDMapping(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	for i := 0; i < len(data); {
		r, n := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && n <= 1 {
			return "", pystr.UTF8DecodeError(data, i)
		}
		i += n
	}
	v, err := pyjson.Loads(string(data))
	if err != nil {
		return "", err
	}
	return NormalizeWhatsAppIdentifier(v), nil
}

// Shortest alias of a sender, ties broken by code point order, empty for empty input
func CanonicalWhatsAppIdentifier(ctx context.Context, identifier string) (string, error) {
	normalized := NormalizeWhatsAppIdentifier(identifier)
	if normalized == "" {
		return "", nil
	}
	aliases, err := expandWhatsAppAliases(ctx, normalized)
	if err != nil {
		return "", err
	}
	best := ""
	for a := range aliases {
		la, lb := utf8.RuneCountInString(a), utf8.RuneCountInString(best)
		if best == "" || la < lb || (la == lb && a < best) {
			best = a
		}
	}
	return best, nil
}
