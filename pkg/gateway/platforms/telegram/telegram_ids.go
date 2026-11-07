package telegram

import (
	"math/big"
	"regexp"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var telegramUsernameRE = regexp.MustCompile(`^@[A-Za-z0-9_]{4,32}$`)

const sendTargetPattern = `^\s*(-?\d+)(?::(\d+))?\s*$`

var sendTargetParser = gateway.RegexGroupsParser(sendTargetPattern, true)

// Bot API chat id, an int64 or big int for numeric text and the stripped string otherwise
func NormalizeTelegramChatID(chatID any) any {
	text := pystr.Strip(pyrepr.Str(chatID))
	n, ok := pystr.IntText(text)
	if !ok {
		return text
	}
	if n.IsInt64() {
		return n.Int64()
	}
	return n
}

// Whether the stripped text of the value is an at sign username of four to thirty two word characters
func LooksLikeTelegramUsername(chatID any) bool {
	return telegramUsernameRE.MatchString(pystr.Strip(pyrepr.Str(chatID)))
}

// Stripped target when it is an at sign username, false otherwise
func ParseTelegramUsernameTarget(targetRef any) (string, bool) {
	value := pystr.Strip(pyrepr.Str(targetRef))
	if LooksLikeTelegramUsername(value) {
		return value, true
	}
	return "", false
}

// Target syntax of the Telegram platform entry, a numeric chat with optional topic or an at sign username
func ParseSendTarget(ref string) any {
	if parsed := sendTargetParser(ref); parsed != nil {
		return parsed
	}
	username, _ := ParseTelegramUsernameTarget(ref)
	return gateway.ParseNonempty(username)
}

// Reply anchor as a positive Telegram message id, nil for a bool, a non positive int or any other text
func ParseTelegramMessageID(value any) *big.Int {
	if _, ok := value.(bool); ok {
		return nil
	}
	if n, ok := pyIntValue(value); ok {
		if n.Sign() > 0 {
			return n
		}
		return nil
	}
	text := ""
	if value != nil {
		text = pystr.Strip(pyrepr.Str(value))
	}
	if text == "" {
		return nil
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return nil
		}
	}
	n, _ := new(big.Int).SetString(text, 10)
	if n.Sign() > 0 {
		return n
	}
	return nil
}

// Integer value of a Go integer kind or big int, the isinstance int check of hermes
func pyIntValue(value any) (*big.Int, bool) {
	switch v := value.(type) {
	case int:
		return big.NewInt(int64(v)), true
	case int8:
		return big.NewInt(int64(v)), true
	case int16:
		return big.NewInt(int64(v)), true
	case int32:
		return big.NewInt(int64(v)), true
	case int64:
		return big.NewInt(v), true
	case uint:
		return new(big.Int).SetUint64(uint64(v)), true
	case uint8:
		return big.NewInt(int64(v)), true
	case uint16:
		return big.NewInt(int64(v)), true
	case uint32:
		return big.NewInt(int64(v)), true
	case uint64:
		return new(big.Int).SetUint64(v), true
	case *big.Int:
		if v == nil {
			return nil, false
		}
		return new(big.Int).Set(v), true
	}
	return nil, false
}
