// Keybinds every legend, palette and menu derives its keys from, bound per context and kept in one host file
package keys

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
)

// Something a key may fire, handled by the manager, the app or the focused card depending on its context
type Action struct {
	ID      string
	Context string
	// Word shown beside the key in legends
	Label string
	// One line for the editor
	Doc     string
	Default []string
	// Fires while an overlay holds the input
	Overlay bool
	// Stays out of the palette, for editing and movement keys
	Quiet bool
}

// Keybinds in force, with the registry they are drawn from and the file they are kept in
type Map struct {
	actions  []Action
	index    map[string]int
	binds    map[string][]string
	byChord  map[string]map[string][]string
	path     string
	stamp    time.Time
	contexts []string
}

// Sent to every card once a bind changed, so inputs rebuild the bindings they hold
type ChangedMsg struct{}

// Map holding every registered action at its default keys
func Default() *Map {
	m := &Map{index: map[string]int{}, binds: map[string][]string{}}
	for _, a := range registry {
		m.Register(a)
	}
	return m
}

// Adds an action to the registry at its default keys, replacing one with the same id
func (m *Map) Register(a Action) {
	if a.Context == "" {
		a.Context = Global
	}
	if !slices.Contains(m.contexts, a.Context) {
		m.contexts = append(m.contexts, a.Context)
	}
	chords := make([]string, 0, len(a.Default))
	for _, d := range a.Default {
		c, err := Parse(d)
		if err != nil {
			panic("keys: default of " + a.ID + ": " + err.Error())
		}
		chords = append(chords, c)
	}
	a.Default = chords
	if i, ok := m.index[a.ID]; ok {
		m.actions[i] = a
	} else {
		m.index[a.ID] = len(m.actions)
		m.actions = append(m.actions, a)
	}
	m.binds[a.ID] = slices.Clone(chords)
	m.byChord = nil
}

// Every action in registry order
func (m *Map) Actions() []Action { return slices.Clone(m.actions) }

// Action with the id
func (m *Map) Action(id string) (Action, bool) {
	i, ok := m.index[id]
	if !ok {
		return Action{}, false
	}
	return m.actions[i], true
}

// Contexts in registry order
func (m *Map) Contexts() []string { return slices.Clone(m.contexts) }

// Chords bound to the action, none when it is unbound
func (m *Map) Keys(id string) []string { return slices.Clone(m.binds[id]) }

// First chord bound to the action, empty when it is unbound
func (m *Map) First(id string) string {
	if b := m.binds[id]; len(b) > 0 {
		return b[0]
	}
	return ""
}

// First chord of the action formatted for a legend, empty when it is unbound
func (m *Map) Display(id string) string { return Format(m.First(id)) }

// Every chord of the action formatted, space separated
func (m *Map) DisplayAll(id string) string {
	out := make([]string, 0, len(m.binds[id]))
	for _, c := range m.binds[id] {
		out = append(out, Format(c))
	}
	return strings.Join(out, " ")
}

// Id of another action in the same context holding the chord, empty when it is free
func (m *Map) Holder(id, chord string) string {
	a, ok := m.Action(id)
	if !ok {
		return ""
	}
	for _, other := range m.actions {
		if other.ID != id && other.Context == a.Context && slices.Contains(m.binds[other.ID], chord) {
			return other.ID
		}
	}
	return ""
}

// Replaces every chord of the action with the given ones
func (m *Map) Set(id string, chords []string) error {
	if _, ok := m.Action(id); !ok {
		return fmt.Errorf("keys: unknown action %q", id)
	}
	out := make([]string, 0, len(chords))
	for _, raw := range chords {
		c, err := Parse(raw)
		if err != nil {
			return err
		}
		if h := m.Holder(id, c); h != "" {
			return fmt.Errorf("keys: %s is bound to %s", Format(c), h)
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	m.binds[id] = out
	m.byChord = nil
	return nil
}

// Binds the action to the one chord
func (m *Map) Rebind(id, chord string) error { return m.Set(id, []string{chord}) }

// Adds a chord to the action
func (m *Map) Add(id, chord string) error {
	return m.Set(id, append(m.Keys(id), chord))
}

// Leaves the action without keys
func (m *Map) Unbind(id string) error { return m.Set(id, nil) }

// Returns the action to its default keys
func (m *Map) Reset(id string) error {
	a, ok := m.Action(id)
	if !ok {
		return fmt.Errorf("keys: unknown action %q", id)
	}
	return m.Set(id, a.Default)
}

// Returns every action to its default keys
func (m *Map) defaults() {
	for _, a := range m.actions {
		m.binds[a.ID] = slices.Clone(a.Default)
	}
	m.byChord = nil
}

// Reports whether the action holds its default keys
func (m *Map) AtDefault(id string) bool {
	a, ok := m.Action(id)
	return ok && slices.Equal(m.binds[id], a.Default)
}

func (m *Map) rebuild() {
	m.byChord = map[string]map[string][]string{}
	for _, a := range m.actions {
		ctx := m.byChord[a.Context]
		if ctx == nil {
			ctx = map[string][]string{}
			m.byChord[a.Context] = ctx
		}
		for _, c := range m.binds[a.ID] {
			ctx[c] = append(ctx[c], a.ID)
		}
	}
}

// Actions bound to the key press, the contexts in order and then the global context
func (m *Map) Lookup(contexts []string, msg tea.KeyPressMsg) []string {
	if m.byChord == nil {
		m.rebuild()
	}
	chords := Chords(msg)
	if len(chords) == 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, ctx := range append(slices.Clone(contexts), Global) {
		if seen[ctx] {
			continue
		}
		seen[ctx] = true
		for _, c := range chords {
			for _, id := range m.byChord[ctx][c] {
				if !slices.Contains(out, id) {
					out = append(out, id)
				}
			}
		}
	}
	return out
}

// Actions of the contexts in registry order, the global ones last
func (m *Map) InContexts(contexts []string) []Action {
	var out []Action
	for _, ctx := range append(slices.Clone(contexts), Global) {
		for _, a := range m.actions {
			if a.Context == ctx {
				out = append(out, a)
			}
		}
	}
	return out
}

var modOrder = []string{"ctrl", "alt", "shift", "super", "meta", "hyper"}

var modBits = map[string]tea.KeyMod{"ctrl": tea.ModCtrl, "alt": tea.ModAlt, "shift": tea.ModShift, "super": tea.ModSuper, "meta": tea.ModMeta, "hyper": tea.ModHyper}

var modAliases = map[string]string{"control": "ctrl", "c": "ctrl", "option": "alt", "opt": "alt", "a": "alt", "s": "shift", "cmd": "super", "win": "super", "logo": "super", "mod": "alt"}

var special = map[string]rune{
	"enter": tea.KeyEnter, "tab": tea.KeyTab, "backspace": tea.KeyBackspace, "esc": tea.KeyEscape, "space": tea.KeySpace,
	"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight, "insert": tea.KeyInsert, "delete": tea.KeyDelete,
	"pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown, "home": tea.KeyHome, "end": tea.KeyEnd, "begin": tea.KeyBegin, "find": tea.KeyFind, "select": tea.KeySelect,
	"menu": tea.KeyMenu, "pause": tea.KeyPause, "printscreen": tea.KeyPrintScreen,
	"f1": tea.KeyF1, "f2": tea.KeyF2, "f3": tea.KeyF3, "f4": tea.KeyF4, "f5": tea.KeyF5, "f6": tea.KeyF6, "f7": tea.KeyF7, "f8": tea.KeyF8, "f9": tea.KeyF9, "f10": tea.KeyF10, "f11": tea.KeyF11, "f12": tea.KeyF12,
	"f13": tea.KeyF13, "f14": tea.KeyF14, "f15": tea.KeyF15, "f16": tea.KeyF16, "f17": tea.KeyF17, "f18": tea.KeyF18, "f19": tea.KeyF19, "f20": tea.KeyF20, "f21": tea.KeyF21, "f22": tea.KeyF22, "f23": tea.KeyF23, "f24": tea.KeyF24,
}

var keyAliases = map[string]string{"escape": "esc", "return": "enter", "pgdn": "pgdown", "pagedown": "pgdown", "pageup": "pgup", "del": "delete", "ins": "insert", "bs": "backspace", "spc": "space", "plus": "+", "minus": "-", "comma": ",", "period": ".", "slash": "/", "backslash": "\\", "equal": "=", "equals": "="}

// Keypad codes folded onto the keys they stand for
var keypad = map[rune]rune{tea.KeyKpEnter: tea.KeyEnter, tea.KeyKpUp: tea.KeyUp, tea.KeyKpDown: tea.KeyDown, tea.KeyKpLeft: tea.KeyLeft, tea.KeyKpRight: tea.KeyRight, tea.KeyKpPgUp: tea.KeyPgUp, tea.KeyKpPgDown: tea.KeyPgDown, tea.KeyKpHome: tea.KeyHome, tea.KeyKpEnd: tea.KeyEnd, tea.KeyKpInsert: tea.KeyInsert, tea.KeyKpDelete: tea.KeyDelete, tea.KeyKpBegin: tea.KeyBegin, tea.KeyKpEqual: '=', tea.KeyKpMultiply: '*', tea.KeyKpPlus: '+', tea.KeyKpComma: ',', tea.KeyKpMinus: '-', tea.KeyKpDecimal: '.', tea.KeyKpDivide: '/', tea.KeyKp0: '0', tea.KeyKp1: '1', tea.KeyKp2: '2', tea.KeyKp3: '3', tea.KeyKp4: '4', tea.KeyKp5: '5', tea.KeyKp6: '6', tea.KeyKp7: '7', tea.KeyKp8: '8', tea.KeyKp9: '9'}

var specialNames = func() map[rune]string {
	out := map[rune]string{}
	for n, r := range special {
		out[r] = n
	}
	return out
}()

// Symbols the shift key produces on a US layout, so shift+= and + name the same chord
var shifted = map[rune]rune{'`': '~', '1': '!', '2': '@', '3': '#', '4': '$', '5': '%', '6': '^', '7': '&', '8': '*', '9': '(', '0': ')', '-': '_', '=': '+', '[': '{', ']': '}', '\\': '|', ';': ':', '\'': '"', ',': '<', '.': '>', '/': '?'}

// Modifier and key of a chord, the key a special name or one printable rune
type parts struct {
	mods tea.KeyMod
	name string
	r    rune
}

// Canonical chord of the parts, letters lower case under shift and US symbols folded onto their shifted form
func canon(mods tea.KeyMod, name string, r rune) string {
	mods &= tea.ModCtrl | tea.ModAlt | tea.ModShift | tea.ModSuper | tea.ModMeta | tea.ModHyper
	if name == "" {
		switch {
		case unicode.IsUpper(r):
			r = unicode.ToLower(r)
			mods |= tea.ModShift
		case mods&tea.ModShift != 0 && !unicode.IsLetter(r):
			if s, ok := shifted[r]; ok {
				r = s
				mods &^= tea.ModShift
			}
		}
	}
	var sb strings.Builder
	for _, m := range modOrder {
		if mods&modBits[m] != 0 {
			sb.WriteString(m)
			sb.WriteByte('+')
		}
	}
	if name != "" {
		sb.WriteString(name)
	} else {
		sb.WriteRune(r)
	}
	return sb.String()
}

// Canonical form of a chord written by a person, such as Alt+Shift+L, G, ctrl+pgdn or alt++
func Parse(s string) (string, error) {
	p, err := parse(s)
	if err != nil {
		return "", err
	}
	return canon(p.mods, p.name, p.r), nil
}

func parse(s string) (parts, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return parts{}, fmt.Errorf("keys: empty chord")
	}
	var p parts
	key := ""
	body := raw
	switch {
	case body == "+":
		key = "+"
		body = ""
	case strings.HasSuffix(body, "++"):
		key = "+"
		body = strings.TrimSuffix(body, "++")
	}
	var fields []string
	if body != "" {
		fields = strings.Split(body, "+")
	}
	if key == "" {
		if len(fields) == 0 {
			return parts{}, fmt.Errorf("keys: %q names no key", raw)
		}
		key = fields[len(fields)-1]
		fields = fields[:len(fields)-1]
	}
	for _, f := range fields {
		m := strings.ToLower(strings.TrimSpace(f))
		if a, ok := modAliases[m]; ok {
			m = a
		}
		bit, ok := modBits[m]
		if !ok {
			return parts{}, fmt.Errorf("keys: %q is not a modifier in %q", f, raw)
		}
		p.mods |= bit
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return parts{}, fmt.Errorf("keys: %q names no key", raw)
	}
	if a, ok := keyAliases[strings.ToLower(key)]; ok {
		key = a
	}
	runes := []rune(key)
	if len(runes) == 1 {
		if !unicode.IsPrint(runes[0]) || runes[0] == ' ' {
			return parts{}, fmt.Errorf("keys: %q is not a printable key in %q", key, raw)
		}
		p.r = runes[0]
		return p, nil
	}
	name := strings.ToLower(key)
	if _, ok := special[name]; !ok {
		return parts{}, fmt.Errorf("keys: unknown key %q in %q", key, raw)
	}
	p.name = name
	return p, nil
}

// Every canonical chord a key press may stand for, the shifted symbol and the base layout key included
func Chords(msg tea.KeyPressMsg) []string {
	var out []string
	add := func(mods tea.KeyMod, code rune) {
		if k, ok := keypad[code]; ok {
			code = k
		}
		var c string
		if name, ok := specialNames[code]; ok {
			c = canon(mods, name, 0)
		} else if unicode.IsPrint(code) && code != ' ' {
			c = canon(mods, "", code)
		} else {
			return
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	mods := msg.Mod &^ (tea.ModCapsLock | tea.ModNumLock | tea.ModScrollLock)
	if msg.Code == tea.KeyExtended {
		return nil
	}
	add(mods, msg.Code)
	if msg.ShiftedCode != 0 {
		add(mods&^tea.ModShift, msg.ShiftedCode)
	}
	if text := []rune(msg.Text); len(text) == 1 && mods&^tea.ModShift == 0 {
		add(0, text[0])
	}
	if msg.BaseCode != 0 {
		add(mods, msg.BaseCode)
	}
	return out
}

// Chord a key press is stored as when captured, the shifted symbol over the base key
func Captured(msg tea.KeyPressMsg) (string, bool) {
	switch msg.Code {
	case tea.KeyLeftShift, tea.KeyRightShift, tea.KeyLeftAlt, tea.KeyRightAlt, tea.KeyLeftCtrl, tea.KeyRightCtrl, tea.KeyLeftSuper, tea.KeyRightSuper, tea.KeyLeftMeta, tea.KeyRightMeta, tea.KeyLeftHyper, tea.KeyRightHyper, tea.KeyIsoLevel3Shift, tea.KeyIsoLevel5Shift, tea.KeyCapsLock, tea.KeyNumLock, tea.KeyScrollLock:
		return "", false
	}
	chords := Chords(msg)
	if len(chords) == 0 {
		return "", false
	}
	return chords[0], true
}

// Key press a terminal reports for the chord, a shifted letter carrying its capital as text
func Press(chord string) tea.KeyPressMsg {
	p, err := parse(chord)
	if err != nil {
		return tea.KeyPressMsg{}
	}
	p, _ = parse(canon(p.mods, p.name, p.r))
	typed := p.mods&^tea.ModShift == 0
	if p.name != "" {
		k := tea.KeyPressMsg{Code: special[p.name], Mod: p.mods}
		if p.name == "space" && typed {
			k.Text = " "
		}
		return k
	}
	k := tea.KeyPressMsg{Code: p.r, Mod: p.mods}
	if typed {
		text := p.r
		if p.mods&tea.ModShift != 0 && unicode.IsLetter(p.r) {
			text = unicode.ToUpper(p.r)
			k.ShiftedCode = text
		}
		k.Text = string(text)
	}
	return k
}

// Chord as legends and the file spell it, a shifted letter as its capital and other keys by name
func Format(chord string) string {
	if chord == "" {
		return ""
	}
	p, err := parse(chord)
	if err != nil {
		return chord
	}
	if p.name == "" && unicode.IsLetter(p.r) && p.mods == tea.ModShift {
		return string(unicode.ToUpper(p.r))
	}
	return canon(p.mods, p.name, p.r)
}

// One cell symbol of a chord that is a bare navigation key, such as the page up key
func Symbol(chord string) (string, bool) {
	sym, ok := symbols[chord]
	return sym, ok
}

var symbols = map[string]string{"pgup": "⇞", "pgdown": "⇟", "home": "⇱", "end": "⇲", "up": "↑", "down": "↓"}

// Two chords as a pair such as j/k, arrows in reading order, either alone when the other is unbound
func Pair(a, b string) string {
	fa, fb := Format(a), Format(b)
	switch {
	case fa == "":
		return fb
	case fb == "":
		return fa
	case strings.TrimSuffix(fa, "down") == strings.TrimSuffix(fb, "up") && strings.HasSuffix(fa, "down"), strings.TrimSuffix(fa, "right") == strings.TrimSuffix(fb, "left") && strings.HasSuffix(fa, "right"):
		fa, fb = fb, fa
	}
	return fa + "/" + fb
}
