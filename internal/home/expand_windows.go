package home

import (
	"os"
	"path/filepath"
	"strings"
)

// Name of the default home directory below the local application data folder
const defaultDirName = "icebath"

// Characters a $NAME reference may hold on Windows
const referenceChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-"

// Variable a foreign Windows environment names its account home with
const profileVar = "USERPROFILE"

// Windows scratch directories keep the ACL they inherit, a mode carrying no meaning there
const securesScratch = false

// Windows collects exited processes itself, so no zombie is ever listed
const zombiesExist = false

// Reports false, since sudo and a root uid exist only on POSIX
func SudoInvokerDefaultHome() (string, bool) { return "", false }

// Reports false, since Windows keeps no POSIX account database of home directories
func accountDatabaseHome() (string, bool) { return "", false }

// Platform default under LOCALAPPDATA, else under the account AppData Local folder, with the literal suffix
func platformDefaultFrom(lookup func(string) string, userHome func() (string, error)) (string, error) {
	suffix := lookup(envSuffix)
	if base := strings.TrimSpace(lookup("LOCALAPPDATA")); base != "" {
		return filepath.Join(base, defaultDirName+suffix), nil
	}
	h, err := userHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "AppData", "Local", defaultDirName+suffix), nil
}

// Account home from USERPROFILE, else HOMEDRIVE with HOMEPATH, false when neither is set
func userHome() (string, bool) {
	if h, ok := os.LookupEnv("USERPROFILE"); ok {
		return h, true
	}
	p, ok := os.LookupEnv("HOMEPATH")
	if !ok {
		return "", false
	}
	return filepath.Join(os.Getenv("HOMEDRIVE"), p), true
}

// Path with a leading tilde or tilde-user replaced by that account home, unchanged when it cannot be named
func ExpandUser(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	i := strings.IndexAny(path, `/\`)
	if i < 0 {
		i = len(path)
	}
	h, ok := userHome()
	if !ok {
		return path
	}
	if i != 1 {
		target := path[1:i]
		if current := os.Getenv("USERNAME"); target != current {
			if current != filepath.Base(h) {
				return path
			}
			h = filepath.Join(filepath.Dir(h), target)
		}
	}
	return h + path[i:]
}

// Path with %NAME%, $NAME and ${NAME} references replaced, quoted text, undefined names, %% and $$ kept as cmd writes them
func expandReferences(path string) string {
	if !strings.ContainsAny(path, "$%") {
		return path
	}
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		c := path[i]
		rest := path[i+1:]
		switch {
		case c == '\'':
			end := strings.IndexByte(rest, '\'')
			if end < 0 {
				b.WriteString(path[i:])
				return b.String()
			}
			b.WriteString(path[i : i+end+2])
			i += end + 1
		case c == '%' && strings.HasPrefix(rest, "%"):
			b.WriteByte('%')
			i++
		case c == '%':
			end := strings.IndexByte(rest, '%')
			if end < 0 {
				b.WriteString(path[i:])
				return b.String()
			}
			name := rest[:end]
			if v, ok := os.LookupEnv(name); ok {
				b.WriteString(v)
			} else {
				b.WriteString("%" + name + "%")
			}
			i += end + 1
		case c == '$' && strings.HasPrefix(rest, "$"):
			b.WriteByte('$')
			i++
		case c == '$' && strings.HasPrefix(rest, "{"):
			end := strings.IndexByte(rest[1:], '}')
			if end < 0 {
				b.WriteString(path[i:])
				return b.String()
			}
			name := rest[1 : end+1]
			if v, ok := os.LookupEnv(name); ok {
				b.WriteString(v)
			} else {
				b.WriteString("${" + name + "}")
			}
			i += end + 2
		case c == '$':
			n := 0
			for n < len(rest) && strings.IndexByte(referenceChars, rest[n]) >= 0 {
				n++
			}
			if v, ok := os.LookupEnv(rest[:n]); ok {
				b.WriteString(v)
			} else {
				b.WriteString("$" + rest[:n])
			}
			i += n
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Reports whether two cleaned paths name the same location on a case insensitive filesystem
func samePath(a, b string) bool { return strings.EqualFold(a, b) }

// Path lower cased with forward slashes turned into backslashes, the form two spellings of one home share
func normCase(p string) string { return strings.ReplaceAll(strings.ToLower(p), "/", `\`) }
