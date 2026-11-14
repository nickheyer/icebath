package approval

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/shell"
)

type Detector struct {
	homeFolds []homeFold
	userFolds []homeFold
	tempDir   string
	profile   string
	image     string
}

func NewDetector(profileHome string) (*Detector, error) {
	if profileHome == "" {
		return nil, fmt.Errorf("approval detector needs the profile home")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate the running executable: %w", err)
	}
	d := &Detector{
		homeFolds: foldsFor([]string{profileHome, realpath(profileHome)}, "~/.icebath"),
		tempDir:   realpath(os.TempDir()),
		image:     pyLower(filepath.Base(exe)),
	}
	if name, ok := home.ProfileName(profileHome); ok {
		d.profile = name
	}
	if userHome, err := home.UserHome(); err == nil {
		d.userFolds = foldsFor([]string{userHome, realpath(userHome), os.Getenv("HOME")}, "~")
	}
	return d, nil
}

func parserLimitExceeded(command string) bool {
	if runeCount(command) > 128_000 {
		return true
	}
	if runeCount(command) > 4_096 && !strings.ContainsAny(command, ";&|\n") {
		return true
	}
	count := 0
	for i := 0; i < len(command); i++ {
		switch command[i] {
		case ';', '&', '|', '\n':
			count++
		}
	}
	return count >= 25_000
}

func (d *Detector) variants(command string, yield func(string) bool) {
	normalized := d.normalize(maskQuotedNewlines(command))
	grepSafeText, _ := grepSafe(normalized)
	seen := map[string]bool{grepSafeText: true}
	if !yield(grepSafeText) {
		return
	}
	fresh := func(v string) bool {
		if v == "" || seen[v] {
			return false
		}
		seen[v] = true
		return true
	}
	if windowsPathShape(command) {
		if v := d.normalize(maskQuotedNewlines(strings.ReplaceAll(command, `\`, "/"))); fresh(v) && !yield(v) {
			return
		}
	}
	pending := []string{normalized}
	for len(pending) > 0 {
		source := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for _, f := range executionFindings(source) {
			if !f.hasPayload || !fresh(f.payload) {
				continue
			}
			if !yield(f.payload) {
				return
			}
			if marked := markCommandStarts(f.payload, "\n"); marked != f.payload && fresh(marked) && !yield(marked) {
				return
			}
			pending = append(pending, f.payload)
		}
	}
	if marked := markCommandStarts(grepSafeText, "\n"); marked != grepSafeText && fresh(marked) && !yield(marked) {
		return
	}
	if faithful := d.normalize(markCommandStarts(maskQuotedNewlines(command), " \n")); fresh(faithful) && !yield(faithful) {
		return
	}
	var spans []edit
	for _, sp := range wordSpans(normalized) {
		if deob := deobfuscateWord(sp.word); deob != "" && deob != sp.word {
			spans = append(spans, edit{sp.start, sp.end, deob})
		}
	}
	slices.SortStableFunc(spans, func(a, b edit) int {
		if c := cmp.Compare(a.start, b.start); c != 0 {
			return c
		}
		return cmp.Compare(a.end, b.end)
	})
	for len(spans) > 0 {
		var applied, carry []edit
		cursor := 0
		for _, sp := range spans {
			if sp.start < cursor {
				carry = append(carry, sp)
			} else {
				applied = append(applied, sp)
				cursor = sp.end
			}
		}
		if v := splice(normalized, applied); fresh(v) && !yield(v) {
			return
		}
		spans = carry
	}
}

func windowsPathShape(command string) bool {
	for i := 0; i+2 < len(command); i++ {
		if isASCIILetter(command[i]) && command[i+1] == ':' && command[i+2] == '\\' {
			return true
		}
		if command[i] == '\\' && command[i+1] == '\\' && command[i+2] == '\\' {
			return true
		}
	}
	return false
}

func (d *Detector) Hardline(command string) (bool, string) {
	if parserLimitExceeded(command) {
		return true, parserLimitDesc
	}
	if _, malformed := quotedGrepSpans(maskQuotedNewlines(command)); malformed {
		return true, malformedExecDesc
	}
	hit, desc := false, ""
	d.variants(command, func(v string) bool {
		s := newSubject(pyLower(v))
		var masked *subject
		for k := range hardlineRules {
			r := &hardlineRules[k]
			target := s
			if r.masked {
				if masked == nil {
					if containsShellCarrier(v) {
						masked = s
					} else {
						masked = newSubject(pyLower(maskQuotedProse(v)))
					}
				}
				target = masked
			}
			if r.hit(target) {
				hit, desc = true, r.desc
				return false
			}
		}
		return true
	})
	return hit, desc
}

func (d *Detector) verificationCleanup(command string) bool {
	argv, err := shell.Split(command)
	if err != nil || len(argv) != 3 || argv[0] != "rm" || argv[1] != "-f" {
		return false
	}
	operand := argv[2]
	base := pyBasename(operand)
	joined := d.tempDir
	if !strings.HasSuffix(joined, pathSep) {
		joined += pathSep
	}
	joined += base
	dir, _ := splitPath(realpath(operand))
	return operand == joined && dir == d.tempDir && verificationNameRe.MatchString(base)
}

func (d *Detector) dangerous(command string, only map[string]bool, yield func(string) bool) {
	if parserLimitExceeded(command) {
		if only == nil {
			yield(parserLimitDesc)
		}
		return
	}
	if d.verificationCleanup(command) {
		return
	}
	stopped := false
	d.variants(command, func(v string) bool {
		s := newSubject(lowerPreservingFlags(v))
		var masked *subject
		for k := range dangerousRules {
			r := &dangerousRules[k]
			if only != nil && !only[r.desc] {
				continue
			}
			target := s
			if r.masked {
				if masked == nil {
					masked = newSubject(lowerPreservingFlags(maskQuotedProse(v)))
				}
				target = masked
			}
			if r.hit(target) && !yield(r.desc) {
				stopped = true
				return false
			}
		}
		return true
	})
	if stopped {
		return
	}
	if only != nil {
		if only[invisibleUnicodeDesc] && hiddenVariationSelector(command) {
			yield(invisibleUnicodeDesc)
		}
		return
	}
	for _, f := range executionFindings(d.normalize(command)) {
		if !yield(f.desc) {
			return
		}
	}
	if d.GatewayLifecycle(command) && !yield(gatewaySpliceDesc) {
		return
	}
	if hiddenVariationSelector(command) {
		yield(invisibleUnicodeDesc)
	}
}

func (d *Detector) Dangerous(command string) (bool, string, string) {
	found := ""
	d.dangerous(command, nil, func(desc string) bool {
		found = desc
		return false
	})
	if found == "" {
		return false, "", ""
	}
	return true, found, found
}

func (d *Detector) ContentRuleKeys(command string) []string {
	var keys []string
	d.dangerous(command, contentRuleDescs, func(desc string) bool {
		if !contains(keys, desc) {
			keys = append(keys, desc)
		}
		return true
	})
	return keys
}

func KeyAliases(key string) []string {
	if aliases, ok := keyAliases[key]; ok {
		return slices.Clone(aliases)
	}
	return []string{key}
}

func (d *Detector) SudoStdin(command string, passwordConfigured bool) (bool, string) {
	if !passwordConfigured && sudoStdinRe.MatchString(wide(pyLower(d.normalize(command)))) {
		return true, "sudo password guessing via stdin (sudo -S)"
	}
	return false, ""
}

func hiddenVariationSelector(command string) bool {
	for i := 0; i < len(command); {
		r, w := runeAt(command, i)
		if (r >= 0xfe00 && r <= 0xfe0f) || (r >= 0xe0100 && r <= 0xe01ef) {
			prev := runeBefore(command, i)
			keycap := r == 0xfe0f && prev >= 0 && strings.ContainsRune("0123456789#*", prev) && nextRune(command, i+w) == 0x20e3
			if !(prev >= 0 && presentationBase(prev)) && !keycap {
				return true
			}
		}
		i += w
	}
	return false
}

func presentationBase(r rune) bool {
	switch r {
	case 0xa9, 0xae, 0x203c, 0x2049, 0x2122, 0x2139, 0x21a9, 0x21aa, 0x231a, 0x231b, 0x2328, 0x23cf, 0x24c2,
		0x25aa, 0x25ab, 0x25b6, 0x25c0, 0x2934, 0x2935, 0x2b1b, 0x2b1c, 0x2b50, 0x2b55, 0x3030, 0x303d, 0x3297, 0x3299:
		return true
	}
	return (r >= 0x2194 && r <= 0x2199) || (r >= 0x23e9 && r <= 0x23f3) || (r >= 0x23f8 && r <= 0x23fa) ||
		(r >= 0x25fb && r <= 0x25fe) || (r >= 0x2b05 && r <= 0x2b07) || emojiRange(r)
}

func (d *Detector) denyVariants(command string, yield func(string) bool) {
	stopped := false
	d.variants(command, func(v string) bool {
		if !yield(v) {
			stopped = true
			return false
		}
		return true
	})
	if stopped {
		return
	}
	pending := []string{command}
	seen := map[string]bool{}
	for len(pending) > 0 {
		source := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[source] {
			continue
		}
		seen[source] = true
		for _, sp := range wordSpans(source) {
			segment := shellCommandSegment(source, sp.start)
			executable := deobfuscateWord(sp.word)
			tail := pySlice(segment, sp.end-sp.start, len(segment))
			var parts strings.Builder
			lastSpace := false
			for st := range scanShell(tail, 0, len(tail), scanMode{}) {
				if st.kind == stepChar && st.quote == 0 && spaceAt(tail, st.i) {
					if parts.Len() == 0 || !lastSpace {
						parts.WriteByte(' ')
						lastSpace = true
					}
					continue
				}
				parts.WriteString(tail[st.i:st.j])
				lastSpace = false
			}
			tail = parts.String()
			names := []string{executable}
			if base := pyBasename(executable); base != executable {
				names = append(names, base)
			}
			for _, name := range names {
				candidate := name + tail
				if !yield(candidate) || !yield(d.normalize(candidate)) {
					return
				}
			}
			if pyBasename(executable) == "env" {
				if tokens, ok := segmentTokens(segment, 0); ok && len(tokens) > 0 {
					if payload, ok := envSplitPayload(tokens); ok && payload != "" {
						pending = append(pending, payload)
					}
				}
			}
		}
		for _, f := range executionFindings(source) {
			if f.hasPayload && f.payload != "" {
				pending = append(pending, f.payload)
			}
		}
	}
}
