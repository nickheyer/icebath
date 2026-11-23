package roundtrip

import (
	"fmt"
	"sort"
	"strings"
)

// Shell PATH the dotenv properties boot with
const BasePath = "/usr/bin:/bin"

// Dotenv text with quoting, self references, chains and placeholders, its keys and the shell exports before load
func GenDotenv(seed int64) (string, []string, map[string]string) {
	g := NewRNG(seed)
	p := fmt.Sprintf("C18_%d_", seed)
	shell := map[string]string{p + "SELF": "shellval", p + "SHADOW": "from-shell"}
	entries := []string{
		p + "A=plain-" + Choice(g, Words),
		fmt.Sprintf(`export %sB="double quoted # not a comment %s"`, p, Choice(g, Unicode)),
		p + "C='single ${NOT_EXPANDED} $HOME'",
		p + "D=${" + p + "A}-chained",
		fmt.Sprintf("%sE=${C18_UNDEFINED_%d}", p, seed),
		fmt.Sprintf("PATH=/c18/%d/bin:${PATH}", seed),
		p + "SELF=pre-${" + p + "SELF}",
		p + "SP = spaced value",
		p + "U=" + Choice(g, Unicode),
		p + "EQ=a=b=c",
		p + "SHADOW=from-dotenv",
		p + `ESC="tab\tnewline\n"`,
		p + "CHAIN2=${" + p + "D}/${" + p + "U}",
	}
	Shuffle(g, entries)
	keyOf := func(e string) string {
		k, _, _ := strings.Cut(e, "=")
		return strings.TrimSpace(strings.ReplaceAll(k, "export ", ""))
	}
	rank := map[string]int{p + "A": 0, p + "U": 0, p + "D": 1, p + "CHAIN2": 2}
	sort.SliceStable(entries, func(i, j int) bool { return rank[keyOf(entries[i])] < rank[keyOf(entries[j])] })
	lines := []string{"# C18 generated .env", ""}
	keySet := map[string]bool{}
	for _, e := range entries {
		if g.Random() < 0.2 {
			lines = append(lines, "# note")
		}
		lines = append(lines, e)
		keySet[keyOf(e)] = true
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(lines, "\n") + "\n", keys, shell
}
