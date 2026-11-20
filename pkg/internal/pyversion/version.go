// Package pyversion parses and orders versions the way packaging.version.Version does
package pyversion

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var versionRe = regexp.MustCompile(`(?i)^v?(?:(?:([0-9]+)!)?([0-9]+(?:\.[0-9]+)*)` +
	`([-_.]?(alpha|a|beta|b|preview|pre|c|rc)[-_.]?([0-9]+)?)?` +
	`((?:-([0-9]+))|(?:[-_.]?(post|rev|r)[-_.]?([0-9]+)?))?` +
	`([-_.]?(dev)[-_.]?([0-9]+)?)?)(?:\+([a-z0-9]+(?:[-_.][a-z0-9]+)*))?$`)

var localSepRe = regexp.MustCompile(`[._-]`)

// InvalidVersion packaging raises for text outside the PEP 440 grammar
type InvalidVersion struct{ Text string }

func (e *InvalidVersion) Error() string {
	return fmt.Sprintf("Invalid version: %s", pyrepr.StrRepr(e.Text))
}

// Parsed PEP 440 version with the packaging normalisation applied
type Version struct {
	Epoch   *big.Int
	Release []*big.Int
	PreL    string
	PreN    *big.Int
	Post    *big.Int
	Dev     *big.Int
	Local   []string
}

// Parses text like packaging.version.Version, an InvalidVersion for text outside the grammar
func Parse(text string) (*Version, error) {
	m := versionRe.FindStringSubmatch(pystr.Strip(text))
	if m == nil {
		return nil, &InvalidVersion{Text: text}
	}
	v := &Version{Epoch: num(m[1])}
	for _, part := range strings.Split(m[2], ".") {
		v.Release = append(v.Release, num(part))
	}
	if m[4] != "" {
		v.PreL = normalizePre(strings.ToLower(m[4]))
		v.PreN = num(m[5])
	}
	if m[6] != "" {
		if m[7] != "" {
			v.Post = num(m[7])
		} else {
			v.Post = num(m[9])
		}
	}
	if m[10] != "" {
		v.Dev = num(m[12])
	}
	if m[13] != "" {
		v.Local = localSepRe.Split(strings.ToLower(m[13]), -1)
	}
	return v, nil
}

// Integer of a digit run, zero when the run is empty
func num(digits string) *big.Int {
	n := new(big.Int)
	if digits != "" {
		n.SetString(digits, 10)
	}
	return n
}

// Canonical pre release letter of a spelling
func normalizePre(letter string) string {
	switch letter {
	case "alpha":
		return "a"
	case "beta":
		return "b"
	case "c", "pre", "preview":
		return "rc"
	}
	return letter
}

// Sign of a minus b in the packaging _cmpkey order
func Compare(a, b *Version) int {
	if c := a.Epoch.Cmp(b.Epoch); c != 0 {
		return c
	}
	if c := compareRelease(trimZeros(a.Release), trimZeros(b.Release)); c != 0 {
		return c
	}
	if c := comparePre(a, b); c != 0 {
		return c
	}
	if c := compareOptional(a.Post, b.Post, -1); c != 0 {
		return c
	}
	if c := compareOptional(a.Dev, b.Dev, 1); c != 0 {
		return c
	}
	return compareLocal(a.Local, b.Local)
}

// Release segment without its trailing zeros
func trimZeros(release []*big.Int) []*big.Int {
	end := len(release)
	for end > 0 && release[end-1].Sign() == 0 {
		end--
	}
	return release[:end]
}

// Lexicographic order of two integer tuples
func compareRelease(a, b []*big.Int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := a[i].Cmp(b[i]); c != 0 {
			return c
		}
	}
	return sign(len(a) - len(b))
}

// Pre release order where a bare dev release sorts first and a final release sorts last
func comparePre(a, b *Version) int {
	rank := func(v *Version) int {
		switch {
		case v.PreL == "" && v.Post == nil && v.Dev != nil:
			return -1
		case v.PreL == "":
			return 1
		}
		return 0
	}
	ra, rb := rank(a), rank(b)
	if ra != 0 || rb != 0 {
		return sign(ra - rb)
	}
	if c := strings.Compare(a.PreL, b.PreL); c != 0 {
		return c
	}
	return a.PreN.Cmp(b.PreN)
}

// Order of two optional numbers where absence ranks as missing (minus or plus infinity)
func compareOptional(a, b *big.Int, missing int) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return missing
	case b == nil:
		return -missing
	}
	return a.Cmp(b)
}

// Local label order where numeric parts outrank alphanumeric ones and no label sorts first
func compareLocal(a, b []string) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		na, aNum := new(big.Int).SetString(a[i], 10)
		nb, bNum := new(big.Int).SetString(b[i], 10)
		var c int
		switch {
		case aNum && bNum:
			c = na.Cmp(nb)
		case aNum:
			c = 1
		case bNum:
			c = -1
		default:
			c = strings.Compare(a[i], b[i])
		}
		if c != 0 {
			return c
		}
	}
	return sign(len(a) - len(b))
}

// Sign of an integer
func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
