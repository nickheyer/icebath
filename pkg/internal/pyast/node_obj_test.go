package pyast

import (
	"math/big"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Checks the Python class names of the constant kinds and the digit limit of ints inside tuples and frozensets
func TestConstantTypeNamesAndTupleDigitLimit(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{{Complex{1, 2}, "complex"}, {Ellipsis{}, "ellipsis"}, {FrozenSet{}, "frozenset"}, {pyrepr.Tuple{}, "tuple"}} {
		if got := pyrepr.TypeName(c.v); got != c.want {
			t.Errorf("TypeName(%#v) = %q, want %q", c.v, got, c.want)
		}
	}
	ok := new(big.Int).Exp(big.NewInt(10), big.NewInt(4299), nil)
	huge := new(big.Int).Exp(big.NewInt(10), big.NewInt(4300), nil)
	if s, err := ConstantRepr(pyrepr.Tuple{ok, 1}); err != nil || len(s) != 4300+5 || !strings.HasSuffix(s, ", 1)") {
		t.Fatalf("4300 digit int in a tuple: %d chars %v", len(s), err)
	}
	for _, v := range []any{huge, pyrepr.Tuple{1, huge}, pyrepr.Tuple{pyrepr.Tuple{new(big.Int).Neg(huge)}}, FrozenSet{huge}} {
		_, err := ConstantRepr(v)
		if err == nil || err.Error() != (&IntDigitsError{}).Error() {
			t.Fatalf("ConstantRepr of a 4301 digit int: %v", err)
		}
	}
}
