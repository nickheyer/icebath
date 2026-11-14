package tui

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestUsageGaugeColoring(t *testing.T) {
	t.Run("maps used fill like the model picker (green / yellow / red)", func(t *testing.T) {
		for bar, want := range map[string]string{
			"[          ]": "green", "[|         ]": "green", "[|||||     ]": "green", "[||||||    ]": "yellow",
			"[||||||||  ]": "yellow", "[||||||||| ]": "red", "[||||||||||]": "red",
		} {
			if got := gaugeBarStyle(bar); got != want {
				t.Fatalf("%s got %s", bar, got)
			}
		}
	})
	t.Run("recognises a gauge row", func(t *testing.T) {
		for text, want := range map[string]bool{
			"Today      [||        ]  24% used": true, "Top-tier   [||||||||||]  100% used": true, "plain system text": false,
			"- [ ] task item": false, "[]": false, "a [|] pipe or [   ] gap": false, "[|||||||||||]": false, "[ |        ]": false,
		} {
			if hasUsageGauge(text) != want {
				t.Fatalf("%q", text)
			}
		}
	})
	t.Run("colors only the bar, leaves the rest of the /usage line alone", func(t *testing.T) {
		got := splitUsageGaugeLine("Today      [||        ]  24% used  \u00b7  resets in 23h")
		want := []gaugeSegment{{"", "Today      "}, {"", "["}, {"green", "||"}, {"", "        "}, {"", "]"}, {"", "  24% used  \u00b7  resets in 23h"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v", got)
		}
	})
}

func TestSessionLabel(t *testing.T) {
	t.Run("shows a canonical durable id as main's 8-character label", func(t *testing.T) {
		if got := sessionLabel("local-ab12cd34" + strings.Repeat("0", 56)); got != "ab12cd34" {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("leaves an 8-character label as it is", func(t *testing.T) {
		if sessionLabel("a707d4c3") != "a707d4c3" || sessionLabel("") != "" {
			t.Fatal("changed")
		}
	})
	t.Run("shows a resumed stored id as a stable 8-character label", func(t *testing.T) {
		label := sessionLabel("20261012_134501_abcdef")
		if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(label) || sessionLabel("20261012_134501_abcdef") != label || sessionLabel("20261012_134501_abcdeg") == label {
			t.Fatalf("label %s", label)
		}
	})
}
