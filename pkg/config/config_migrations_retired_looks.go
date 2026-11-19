package config

import "strings"

// Skins icebath no longer ships, each moved to the built in skin closest to it
var retiredSkins = map[string]string{
	"ares": DefaultSkinName, "mono": DefaultSkinName, "slate": DefaultSkinName, "poseidon": DefaultSkinName,
	"sisyphus": DefaultSkinName, "charizard": DefaultSkinName, "daylight": "light", "warm-lightmode": "light",
}

// Busy indicator style icebath no longer draws
const retiredIndicatorStyle = "kaomoji"

// Moves a config off a skin or busy indicator style icebath no longer ships
func migrateTo67(r *migrationRun) error {
	cfg := r.read()
	display, ok := cfg["display"].(map[string]any)
	if !ok {
		return nil
	}
	var added, notes []string
	if skin, isStr := display["skin"].(string); isStr {
		if next, retired := retiredSkins[strings.ToLower(strings.TrimSpace(skin))]; retired {
			display["skin"] = next
			added = append(added, "display.skin="+next)
			notes = append(notes, "  ✓ Changed display.skin from "+strings.TrimSpace(skin)+" to "+next+", the built in skin that replaces it.")
		}
	}
	if style, isStr := display["tui_status_indicator"].(string); isStr && strings.ToLower(strings.TrimSpace(style)) == retiredIndicatorStyle {
		display["tui_status_indicator"] = "unicode"
		added = append(added, "display.tui_status_indicator=unicode")
		notes = append(notes, "  ✓ Changed display.tui_status_indicator to unicode, the spinner that replaces the kaomoji one.")
	}
	if len(added) == 0 {
		return nil
	}
	cfg["display"] = display
	return r.commit(cfg, strings.Join(added, ", "), strings.Join(notes, "\n"))
}
