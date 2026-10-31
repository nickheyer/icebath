package keys

import (
	"slices"

	tea "charm.land/bubbletea/v2"
)

// Base keys of the Option rows below, in US layout order
const optionBase = "1234567890-=qwertyuiop[]\\asdfghjkl;'zxcvbnm,./"

// Characters the Option key types over the base keys on a US Mac keyboard
const optionRow = "¡™£¢∞§¶•ªº–≠œ∑´®†¥¨ˆøπ“‘«åß∂ƒ©˙∆˚¬…æΩ≈ç√∫˜µ≤≥÷"

// Characters Option and shift together type over the base keys on a US Mac keyboard
const optionShiftRow = "⁄€‹›ﬁﬂ‡°·‚—±Œ„´‰ˇÁ¨ˆØ∏”’»ÅÍÎÏ˝ÓÔÒÚÆ¸˛Ç◊ı˜Â¯˘¿"

// Alt chords each Option typed character stands for, the unshifted chord first where both rows type it
var option = func() map[rune][]string {
	base, plain, shift := []rune(optionBase), []rune(optionRow), []rune(optionShiftRow)
	if len(plain) != len(base) || len(shift) != len(base) {
		panic("keys: the Option rows do not line up with the base keys")
	}
	out := map[rune][]string{' ': {canon(tea.ModAlt, "space", 0), canon(tea.ModAlt|tea.ModShift, "space", 0)}}
	for i, r := range plain {
		out[r] = append(out[r], canon(tea.ModAlt, "", base[i]))
	}
	for i, r := range shift {
		c := canon(tea.ModAlt|tea.ModShift, "", base[i])
		if !slices.Contains(out[r], c) {
			out[r] = append(out[r], c)
		}
	}
	return out
}()

var optionOn bool

// Turns the reading of Option typed characters as alt chords on or off
func SetOption(on bool) { optionOn = on }

// Reports whether Option typed characters are read as alt chords
func Option() bool { return optionOn }

// Alt chords
func optionChords(msg tea.KeyPressMsg) []string {
	if !optionOn || msg.Mod&^(tea.ModShift|tea.ModCapsLock|tea.ModNumLock|tea.ModScrollLock) != 0 {
		return nil
	}
	if chords, ok := option[msg.ShiftedCode]; ok {
		return chords
	}
	return option[msg.Code]
}

// Reports whether the press is an Option typed character read as an alt chord
func OptionAlt(msg tea.KeyPressMsg) bool { return len(optionChords(msg)) > 0 }
