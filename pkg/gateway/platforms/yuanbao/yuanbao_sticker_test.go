package yuanbao

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func stickerIDs(list []*pyjson.Object) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, stickerField(s, "sticker_id"))
	}
	return out
}

func TestStickerMapShape(t *testing.T) {
	if StickerMap.Len() != 59 || StickerMap.Keys[0] != "六六六" || StickerMap.Keys[58] != "烟花" {
		t.Fatalf("catalogue = %d keys %q", StickerMap.Len(), StickerMap.Keys)
	}
	s := StickerMap.Vals["ok"].(*pyjson.Object)
	got, err := pyjson.Dumps(s, pyjson.EnsureASCII(false))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"sticker_id": "169", "package_id": "1003", "name": "ok", "description": "好的 收到 没问题 okay 行 可以 懂了", "width": 128, "height": 128, "formats": "png"}`
	if got != want {
		t.Fatalf("got %s", got)
	}
}

func TestGetStickerByNameLookupOrder(t *testing.T) {
	for _, c := range []struct{ query, want string }{
		{"六六六", "278"}, {"  害羞 ", "130"}, {"打call啦", "246"}, {"awesome", "278"}, {"厉 害666", "278"}, {"   ", "278"},
	} {
		got := GetStickerByName(c.query)
		if got == nil || stickerField(got, "sticker_id") != c.want {
			t.Fatalf("%q -> %v want %s", c.query, got, c.want)
		}
	}
	if GetStickerByName("") != nil {
		t.Fatal("empty name matched")
	}
}

func TestGetStickerByID(t *testing.T) {
	if s := GetStickerByID(" 278 "); s == nil || stickerField(s, "name") != "六六六" {
		t.Fatalf("got %v", s)
	}
	if GetStickerByID("") != nil || GetStickerByID("999") != nil {
		t.Fatal("unknown id matched")
	}
}

func TestGetRandomStickerPrefersCategory(t *testing.T) {
	allowed := map[string]bool{"278": true, "183": true, "239": true}
	for range 50 {
		if id := stickerField(GetRandomSticker("666"), "sticker_id"); !allowed[id] {
			t.Fatalf("category 666 drew %s", id)
		}
	}
	if GetRandomSticker("") == nil || GetRandomSticker("不存在的词") == nil {
		t.Fatal("no sticker drawn")
	}
}

func TestTextHelpers(t *testing.T) {
	if got := normalizeText("  ＡＢＣ　"); got != "abc" {
		t.Fatalf("normalize = %q", got)
	}
	if got := compactText("a-b_c·d.e,f，g。h!i！j?k？l\"m“n”o'p‘q’r、s/t\\u v　w"); got != "abcdefghijklmnopqrstuvw" {
		t.Fatalf("compact = %q", got)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-12 }
	r := func(s string) []rune { return []rune(s) }
	if !near(multisetCharHitRatio(r("aab"), r("ab")), 2.0/3) || multisetCharHitRatio(nil, r("x")) != 0 {
		t.Fatal("multiset ratio")
	}
	if !near(bigramJaccard(r("abc"), r("bcd")), 1.0/3) || bigramJaccard(r("a"), r("abc")) != 0 {
		t.Fatal("bigram jaccard")
	}
	if longestSubsequenceRatio(r("ace"), r("abcde")) != 1 || !near(longestSubsequenceRatio(r("aec"), r("abcde")), 2.0/3) {
		t.Fatal("subsequence ratio")
	}
	for _, c := range []struct {
		hay, q string
		want   float64
	}{{"打call", "打call", 100}, {"abcdef", "cd", 94}, {"abcdef", "ab", 94}, {"a b", "ab", 86}, {"abc", "b", 93}, {"x", "", 0}, {"", "x", 0}} {
		if got := scoreField(c.hay, c.q); got != c.want {
			t.Fatalf("score(%q, %q) = %v want %v", c.hay, c.q, got, c.want)
		}
	}
}

func TestSearchStickersRanking(t *testing.T) {
	for _, c := range []struct {
		query string
		limit int
		want  []string
	}{
		{"", 3, []string{"278", "262", "130"}},
		{"278", 10, []string{"278"}},
		{"27", 10, []string{"278", "276", "227", "272"}},
		{"厉 害666", 10, []string{"278", "239", "183"}},
		{"ж", 3, []string{"278", "262", "130"}},
		{"cool", 1, []string{"131"}},
	} {
		if got := stickerIDs(SearchStickers(c.query, c.limit)); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%q -> %v want %v", c.query, got, c.want)
		}
	}
	if n := len(SearchStickers("", 0)); n != 10 {
		t.Fatalf("limit 0 gave %d", n)
	}
	if n := len(SearchStickers("", -4)); n != 1 {
		t.Fatalf("negative limit gave %d", n)
	}
	if n := len(SearchStickers("", 1000)); n != 59 {
		t.Fatalf("large limit gave %d", n)
	}
	low := SearchStickers(strings.Repeat("ж", 20)+"o", 2)
	if len(low) != 2 || stickerField(low[0], "name") != "ok" {
		t.Fatalf("low score fallback = %v", stickerIDs(low))
	}
}

func TestBuildStickerMsgBody(t *testing.T) {
	body, err := BuildStickerMsgBody(StickerMap.Vals["六六六"].(*pyjson.Object))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := pyjson.Dumps(body, pyjson.EnsureASCII(false))
	want := `[{"msg_type": "TIMFaceElem", "msg_content": {"index": 0, "data": "{\"sticker_id\":\"278\",\"package_id\":\"1003\",\"width\":128,\"height\":128,\"formats\":\"png\",\"name\":\"六六六\"}"}}]`
	if got != want {
		t.Fatalf("got %s", got)
	}
	bare := pyjson.NewObject()
	bare.Set("name", "x")
	bare.Set("package_id", "1")
	bare.Set("sticker_id", "9")
	body, err = BuildStickerMsgBody(bare)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := body[0].(*pyjson.Object).Vals["msg_content"].(*pyjson.Object).Get("data")
	if data != `{"sticker_id":"9","package_id":"1","width":128,"height":128,"formats":"png","name":"x"}` {
		t.Fatalf("defaults = %v", data)
	}
	bare.Delete("sticker_id")
	var ke *KeyError
	if _, err = BuildStickerMsgBody(bare); !errors.As(err, &ke) || err.Error() != "'sticker_id'" {
		t.Fatalf("missing key = %v", err)
	}
	face, _ := pyjson.Dumps(BuildFaceMsgBody(5, nil))
	if face != `[{"msg_type": "TIMFaceElem", "msg_content": {"index": 5}}]` {
		t.Fatalf("face = %s", face)
	}
}
