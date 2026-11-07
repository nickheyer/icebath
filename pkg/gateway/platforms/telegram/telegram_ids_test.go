package telegram

import (
	"math/big"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestNormalizeReturnsIntOrPassthroughString(t *testing.T) {
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	for _, c := range []struct {
		value, want any
	}{
		{"123456789", int64(123456789)},
		{"-1001234567890", int64(-1001234567890)},
		{123456789, int64(123456789)},
		{"  42 ", int64(42)},
		{"@some_user", "@some_user"},
		{"@a_channel", "@a_channel"},
		{"not_numeric", "not_numeric"},
		{nil, "None"},
		{5.0, "5.0"},
		{"1_000", int64(1000)},
	} {
		if got := NormalizeTelegramChatID(c.value); got != c.want {
			t.Errorf("NormalizeTelegramChatID(%#v) = %#v, want %#v", c.value, got, c.want)
		}
	}
	if got, ok := NormalizeTelegramChatID(huge.String()).(*big.Int); !ok || got.Cmp(huge) != 0 {
		t.Errorf("big chat id = %#v", got)
	}
}

func TestUsernameTargetRecognition(t *testing.T) {
	for _, c := range []struct {
		value any
		want  bool
	}{
		{"@some_user", true}, {" @abcd ", true}, {"@abc", false}, {"@abcdefghijabcdefghijabcdefghijabc", false},
		{"some_user", false}, {"@bad-name", false}, {12345, false}, {"@user\n", true},
	} {
		if got := LooksLikeTelegramUsername(c.value); got != c.want {
			t.Errorf("LooksLikeTelegramUsername(%#v) = %v", c.value, got)
		}
	}
	if v, ok := ParseTelegramUsernameTarget("  @a_channel "); !ok || v != "@a_channel" {
		t.Errorf("username target %q %v", v, ok)
	}
	if _, ok := ParseTelegramUsernameTarget("-100123"); ok {
		t.Error("numeric target parsed as username")
	}
}

func TestParseSendTargetSyntax(t *testing.T) {
	check := func(ref string, chat, thread *string) {
		t.Helper()
		got := ParseSendTarget(ref)
		if chat == nil {
			if got != nil {
				t.Errorf("ParseSendTarget(%q) = %#v, want nil", ref, got)
			}
			return
		}
		p, ok := got.(*gateway.TargetRefParse)
		if !ok || p.ChatID == nil || *p.ChatID != *chat || (thread == nil) != (p.ThreadID == nil) || (thread != nil && *p.ThreadID != *thread) {
			t.Errorf("ParseSendTarget(%q) = %#v", ref, got)
		}
	}
	s := func(v string) *string { return &v }
	check("-1001234567890", s("-1001234567890"), nil)
	check(" 42:7 ", s("42"), s("7"))
	check("@a_channel", s("@a_channel"), nil)
	check(" @a_channel ", s("@a_channel"), nil)
	check("general", nil, nil)
	check("42:topic", nil, nil)
	check("", nil, nil)
}

func TestParseTelegramMessageIDAnchors(t *testing.T) {
	for _, c := range []struct {
		value any
		want  int64
	}{
		{true, 0}, {false, 0}, {42, 42}, {0, 0}, {-3, 0}, {"17", 17}, {" 17 ", 17}, {"0", 0}, {"-5", 0},
		{"+5", 0}, {"１２", 0}, {`{"kind":"loop-tick"}`, 0}, {nil, 0}, {3.0, 0}, {"", 0}, {uint64(9), 9},
	} {
		got := ParseTelegramMessageID(c.value)
		if (got == nil) != (c.want == 0) || (got != nil && got.Int64() != c.want) {
			t.Errorf("ParseTelegramMessageID(%#v) = %v, want %d", c.value, got, c.want)
		}
	}
	if got := ParseTelegramMessageID("99999999999999999999"); got == nil || got.String() != "99999999999999999999" {
		t.Errorf("long anchor = %v", got)
	}
}
