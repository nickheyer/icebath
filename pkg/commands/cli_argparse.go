package commands

import (
	"errors"
	"math"
	"strconv"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/spf13/cobra"
)

// Names the namespace key of a flag the way an argparse dest does
func dest(cmd *cobra.Command, name, key string) {
	_ = cmd.Flags().SetAnnotation(name, "dest", []string{key})
}

// Non negative integer flag value of the backup keep count, a negative keep would drop the newest archives
type nonNegativeKeep int

// Keep value starting at the argparse default
func newNonNegativeKeep(def int) *nonNegativeKeep {
	v := nonNegativeKeep(def)
	return &v
}

func (v *nonNegativeKeep) String() string { return strconv.Itoa(int(*v)) }

func (v *nonNegativeKeep) Type() string { return "int" }

// Accepts decimal digits only, the argparse type error otherwise
func (v *nonNegativeKeep) Set(text string) error {
	refusal := errors.New("must be a non-negative integer (0 keeps everything)")
	if text == "" {
		return refusal
	}
	for _, r := range text {
		if !pystr.IsDecimal(r) {
			return refusal
		}
	}
	n, ok := pyint.Parse(text)
	if !ok || !n.IsInt64() || n.Int64() > math.MaxInt {
		return refusal
	}
	*v = nonNegativeKeep(n.Int64())
	return nil
}
