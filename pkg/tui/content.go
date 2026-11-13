package tui

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
)

// Values of a catalog group in key order, each in the active locale, numbered groups in their numbering
func catalogGroup(group string) []string {
	var keys []string
	for k := range enCatalog.Strings {
		if strings.HasPrefix(k, group+".") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, tr(k))
	}
	return out
}

// Kaomoji spinner frames in catalog order
func faces() []string { return catalogGroup("content.faces") }

// Progress verb of a tool name, false when the catalog has none
func toolVerb(name string) (string, bool) {
	key := "content.verbs." + name
	if _, known := enCatalog.Strings[key]; !known {
		return "", false
	}
	return tr(key), true
}

// Thinking verbs the spinner cycles through
var spinnerVerbs = []string{"pondering", "contemplating", "musing", "cogitating", "ruminating", "deliberating", "mulling", "reflecting", "processing", "reasoning", "analyzing", "computing", "synthesizing", "formulating", "brainstorming"}

// Charms a long running tool row rotates through
var longRunCharms = []string{"still cooking…", "polishing edges…", "asking the void nicely…"}

// Empty composer hints in catalog order
func composerHints() []string { return catalogGroup("composer.placeholders") }

// Slot of the hint picked once per launch so a language switch keeps the same hint
var composerHintSlot = rand.IntN(max(1, len(catalogGroup("composer.placeholders"))))

// Hint of the empty composer, the launch slot or a random one when the active list is shorter
func composerHint() string {
	list := composerHints()
	if composerHintSlot < len(list) {
		return list[composerHintSlot]
	}
	if len(list) == 0 {
		return ""
	}
	return list[rand.IntN(len(list))]
}

// Everyday fortunes in catalog order
func fortunes() []string { return catalogGroup("content.fortunes") }

// Rare fortunes of every twentieth roll
func legendaryFortunes() []string { return catalogGroup("content.legendaryFortunes") }

// FNV style hash over the first UTF-16 unit of each code point, as an unsigned 32 bit value
func fortuneHash(s string) uint32 {
	h := uint32(2166136261)
	for _, r := range s {
		unit := uint32(r)
		if r > 0xffff {
			unit = 0xd800 + (uint32(r)-0x10000)>>10
		}
		h = (h ^ unit) * 16777619
	}
	return h
}

// Fortune of a score, legendary when the score divides by twenty
func fortuneFromScore(n uint32) string {
	rare := n%20 == 0
	bag, mark := fortunes(), "🔮"
	if rare {
		bag, mark = legendaryFortunes(), "🌟"
	}
	if len(bag) == 0 {
		return mark + " undefined"
	}
	return mark + " " + bag[int(n%uint32(len(bag)))]
}

// Fortune of a random roll
func randomFortune() string { return fortuneFromScore(uint32(rand.Float64() * 0x7fffffff)) }

// Fortune fixed for a seed and the local calendar day
func dailyFortune(seed string) string {
	if seed == "" {
		seed = "anon"
	}
	return fortuneFromScore(fortuneHash(seed + "|" + jsDateString(time.Now().In(clockLocation()))))
}

// Date as JavaScript toDateString prints it, weekday, month, two digit day and four digit year
func jsDateString(t time.Time) string {
	year := t.Year()
	y := fmt.Sprintf("%04d", year)
	if year < 0 {
		y = fmt.Sprintf("-%06d", -year)
	}
	return t.Format("Mon Jan 02") + " " + y
}
