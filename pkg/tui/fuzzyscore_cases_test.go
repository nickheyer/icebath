package tui

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tuiserver"
)

func TestNormalizeSlashSearchQuery(t *testing.T) {
	t.Run("trims, strips leading slashes, and lowercases", func(t *testing.T) {
		if tuiserver.NormalizeSlashSearchQuery(" /Model ") != "model" || tuiserver.NormalizeSlashSearchQuery("//help") != "help" || tuiserver.NormalizeSlashSearchQuery("plain") != "plain" {
			t.Fatal("normalize")
		}
	})
}

func TestTokenizeSearchText(t *testing.T) {
	t.Run("returns the full lowercased value plus alphanumeric word tokens", func(t *testing.T) {
		if got := tuiserver.TokenizeSearchText("Commit & Push"); !reflect.DeepEqual(got, []string{"commit & push", "commit", "push"}) {
			t.Fatalf("got %q", got)
		}
	})
}

func TestScoreSlashMenuItem(t *testing.T) {
	item := slashScoreItem{Aliases: []string{"recap", "summary"}, Description: "Turn session recaps on/off", ID: "recaps", Label: "recaps"}
	for _, c := range []struct {
		name, query string
		want        float64
	}{
		{"scores exact name matches at tier 0", "recaps", 0},
		{"scores exact alias matches at tier 0", "summary", 0},
		{"scores name prefixes at tier 1 and name substrings at tier 2", "rec", 1},
		{"scores name prefixes at tier 1 and name substrings at tier 2", "caps", 2},
		{"prefers the name tier when both name and description match", "recap", 0},
		{"returns Infinity when nothing matches", "zzz", math.Inf(1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := scoreSlashMenuItem(item, c.query); got != c.want {
				t.Fatalf("%s got %v", c.query, got)
			}
		})
	}
}

// Slash app entry of the ranking cases
type slashApp struct{ help, id string }

func TestRankSlashItems(t *testing.T) {
	apps := []slashApp{{"Show available commands", "help"}, {"Start a countdown timer", "clock"}, {"Select a model", "models"}}
	toScore := func(a slashApp) slashScoreItem { return slashScoreItem{Description: a.help, ID: a.id} }
	ids := func(list []slashApp) string {
		var out []string
		for _, a := range list {
			out = append(out, a.id)
		}
		return strings.Join(out, ",")
	}
	t.Run("returns the list untouched for an empty query", func(t *testing.T) {
		if ids(rankSlashItems(apps, "/", toScore)) != "help,clock,models" {
			t.Fatal("untouched")
		}
	})
	t.Run("surfaces description matches the prefix filter would miss", func(t *testing.T) {
		if got := ids(rankSlashItems(apps, "/timer", toScore)); got != "clock" {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("ranks name matches above description matches and drops non-matches", func(t *testing.T) {
		if got := ids(rankSlashItems(append([]slashApp{{"model picker widget", "gallery"}}, apps...), "/model", toScore)); got != "models,gallery" {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("keeps original order within a score tier", func(t *testing.T) {
		if got := ids(rankSlashItems([]slashApp{{"", "mod-b"}, {"", "mod-a"}}, "/mod", toScore)); got != "mod-b,mod-a" {
			t.Fatalf("got %s", got)
		}
	})
}
