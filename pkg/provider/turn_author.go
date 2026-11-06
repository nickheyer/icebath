package provider

import (
	"math/big"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	TurnAuthorEnvName       = "ICEBATH_TURN_AUTHOR"
	turnAuthorMaxFieldLen   = 200
	turnAuthorWTF8Surrogate = 0xED
)

var turnAuthorTruthy = map[string]bool{"true": true, "1": true, "yes": true}

// Unassigned code points, read as every rune outside the assigned general categories
func turnAuthorUnassigned(r rune) bool {
	return !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z, unicode.C)
}

// Strips control, surrogate, private and unassigned characters and whitespace, capped at 200 code points
func turnAuthorCleanText(value any) (string, bool) {
	s, ok := value.(string)
	if !ok {
		return "", false
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			if s[i] == turnAuthorWTF8Surrogate && i+2 < len(s) && s[i+1] >= 0xA0 && s[i+1] <= 0xBF {
				i += 3
				continue
			}
			b.WriteByte(s[i])
			i++
			continue
		}
		if !unicode.In(r, unicode.Cc, unicode.Cs, unicode.Co) && !turnAuthorUnassigned(r) {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	text := pystr.Strip(b.String())
	if text == "" {
		return "", false
	}
	if rs := []rune(text); len(rs) > turnAuthorMaxFieldLen {
		text = string(rs[:turnAuthorMaxFieldLen])
	}
	return text, true
}

// Bot flag of a raw author field, truthy strings and nonzero bools or ints
func turnAuthorBotFlag(value any) bool {
	switch v := value.(type) {
	case string:
		return turnAuthorTruthy[pystr.Lower(pystr.Strip(v))]
	case bool:
		return v
	case int:
		return v != 0
	case int64:
		return v != 0
	case *big.Int:
		return v.Sign() != 0
	}
	return false
}

// Bot author id, bot:profile on this install and bot:origin/profile for one on another install
func BotAuthorID(profile, origin string) string {
	if origin = pystr.Strip(origin); origin != "" {
		return "bot:" + origin + "/" + profile
	}
	return "bot:" + profile
}

// Hostname of this machine as an author id origin, cleaned like any author field and empty when unknown
func LocalOrigin() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	text, _ := turnAuthorCleanText(host)
	return strings.ReplaceAll(text, "/", "")
}

// Normalizes a dict or JSON string into id, name and is_bot, nil for anything else or without id and name
func ParseTurnAuthor(raw any) *pyjson.Object {
	switch r := raw.(type) {
	case string:
		parsed, err := pyjson.Loads(r)
		if err != nil {
			return nil
		}
		raw = parsed
	case []byte:
		parsed, err := pyjson.LoadsBytes(r)
		if err != nil {
			return nil
		}
		raw = parsed
	}
	var get func(string) any
	switch m := raw.(type) {
	case *pyjson.Object:
		get = func(k string) any { v, _ := m.Get(k); return v }
	case map[string]any:
		get = func(k string) any { return m[k] }
	default:
		return nil
	}
	id, hasID := turnAuthorCleanText(get("id"))
	name, hasName := turnAuthorCleanText(get("name"))
	if !hasID && !hasName {
		return nil
	}
	if origin, ok := turnAuthorCleanText(get("origin")); ok && hasID && strings.HasPrefix(id, "bot:") && !strings.Contains(id, "/") {
		id, hasID = turnAuthorCleanText(BotAuthorID(id[len("bot:"):], origin))
	}
	author := pyjson.NewObject()
	author.Set("id", nil)
	if hasID {
		author.Set("id", id)
	}
	author.Set("name", nil)
	if hasName {
		author.Set("name", name)
	}
	author.Set("is_bot", turnAuthorBotFlag(get("is_bot")))
	return author
}

// Author the dispatcher placed in the turn author variable of environ, nil environ reading the process environment
func TurnAuthorFromEnv(environ map[string]string) *pyjson.Object {
	if environ == nil {
		v, ok := os.LookupEnv(TurnAuthorEnvName)
		if !ok {
			return nil
		}
		return ParseTurnAuthor(v)
	}
	v, ok := environ[TurnAuthorEnvName]
	if !ok {
		return nil
	}
	return ParseTurnAuthor(v)
}

// Reads and removes the turn author variable so subprocesses started during the turn do not inherit it
func TakeTurnAuthorFromEnv(environ map[string]string) *pyjson.Object {
	author := TurnAuthorFromEnv(environ)
	if environ == nil {
		_ = os.Unsetenv(TurnAuthorEnvName)
	} else {
		delete(environ, TurnAuthorEnvName)
	}
	return author
}

// Environment entry a dispatcher merges into a child env with the compact JSON author
func TurnAuthorEnv(author *pyjson.Object) (map[string]string, error) {
	text, err := pyjson.Dumps(author, pyjson.Separators(",", ":"))
	if err != nil {
		return nil, err
	}
	return map[string]string{TurnAuthorEnvName: text}, nil
}

// Shared a2a name of a bot author turns, empty for a human or an id less bot
func A2AKey(author *pyjson.Object) string {
	if author == nil {
		return ""
	}
	isBot, _ := author.Get("is_bot")
	id, _ := author.Get("id")
	if !pyrepr.Truthy(isBot) || !pyrepr.Truthy(id) {
		return ""
	}
	return "a2a:" + pyrepr.Str(id)
}
