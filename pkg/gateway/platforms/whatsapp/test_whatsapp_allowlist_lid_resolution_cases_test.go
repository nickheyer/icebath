package whatsapp

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

const (
	lidPhone = "351912345678"
	lidID    = "77214955630717"
)

// Writes the forward and reverse lid-mapping files into the legacy session directory
func writeLidMapping(t *testing.T, h string) {
	t.Helper()
	dir := filepath.Join(h, "whatsapp", "session")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"lid-mapping-" + lidPhone + ".json": lidID, "lid-mapping-" + lidID + "_reverse.json": lidPhone} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strconv.Quote(value)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Adapter over the extra with open defaults like the Python harness
func lidAdapter(t *testing.T, extra map[string]any) *WhatsAppAdapter {
	t.Helper()
	for _, k := range []string{"dm_policy", "group_policy"} {
		if _, ok := extra[k]; !ok {
			extra[k] = "open"
		}
	}
	return gatingAdapter(t, extra)
}

func TestDmPhoneAllowlistMatchesLidSender(t *testing.T) {
	writeLidMapping(t, hermeticHome(t))
	a := lidAdapter(t, map[string]any{"dm_policy": "allowlist", "allow_from": []any{lidPhone}})
	if ok, err := a.IsDMAllowed(context.Background(), lidID+"@lid"); err != nil || !ok {
		t.Fatalf("allowed = %v %v", ok, err)
	}
}

func TestDmPhoneWithPlusAllowlistMatchesLidSender(t *testing.T) {
	writeLidMapping(t, hermeticHome(t))
	a := lidAdapter(t, map[string]any{"dm_policy": "allowlist", "allow_from": []any{"+" + lidPhone}})
	if ok, err := a.IsDMAllowed(context.Background(), lidID+"@lid"); err != nil || !ok {
		t.Fatalf("allowed = %v %v", ok, err)
	}
}

func TestGroupJidExactMatchStillWorks(t *testing.T) {
	a := lidAdapter(t, map[string]any{"group_policy": "allowlist", "group_allow_from": []any{"120363001234567890@g.us"}})
	if ok, err := a.IsGroupAllowed(context.Background(), "120363001234567890@g.us", ""); err != nil || !ok {
		t.Fatalf("allowed = %v %v", ok, err)
	}
}

func TestGroupUnlistedJidBlocked(t *testing.T) {
	a := lidAdapter(t, map[string]any{"group_policy": "allowlist", "group_allow_from": []any{"120363001234567890@g.us"}})
	if ok, err := a.IsGroupAllowed(context.Background(), "120363009999999999@g.us", ""); err != nil || ok {
		t.Fatalf("allowed = %v %v", ok, err)
	}
}

func TestShouldProcessMessageDmPhoneAllowlistLidSender(t *testing.T) {
	writeLidMapping(t, hermeticHome(t))
	a := lidAdapter(t, map[string]any{"dm_policy": "allowlist", "allow_from": []any{lidPhone}})
	data := map[string]any{"isGroup": false, "body": "hello", "senderId": lidID + "@lid", "from": lidID + "@lid", "botIds": []any{}, "mentionedIds": []any{}}
	if !gate(t, a, data) {
		t.Fatal("LID sender of an allowlisted phone dropped")
	}
}
