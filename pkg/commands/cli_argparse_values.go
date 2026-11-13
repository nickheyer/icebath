package commands

import (
	"errors"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Flag value refusing a parsed token outside its choices, with the type function message when one is set
type choiceValue struct {
	pflag.Value
	choices []string
	typeMsg string
}

// Converts the token by the flag type, then refuses a converted value or list item outside the choices
func (v *choiceValue) Set(s string) error {
	if err := v.Value.Set(s); err != nil {
		return err
	}
	items := []string{v.Value.String()}
	if sl, ok := v.Value.(pflag.SliceValue); ok {
		items = sl.GetSlice()
	}
	for _, item := range items {
		if !contains(v.choices, item) {
			return v.refusal(item)
		}
	}
	return nil
}

// Argparse text for a refused value, the type function error or the invalid choice line in Python repr
func (v *choiceValue) refusal(value string) error {
	if v.typeMsg != "" {
		return errors.New(v.typeMsg)
	}
	repr := pyrepr.StrRepr
	if t := v.Value.Type(); t == "int" || t == "float64" {
		repr = func(s string) string { return s }
	}
	return errors.New("invalid choice: " + repr(value) + " (choose from " + reprList(v.choices, repr) + ")")
}

// Choice restricted repeatable flag value that keeps the slice accessors the namespace reads
type choiceSliceValue struct {
	*choiceValue
	pflag.SliceValue
}

// Checks a flag of cmd against its choices as each token is parsed, so a refusal is a usage error
func restrictChoices(cmd *cobra.Command, name, typeMsg string, choices []string) {
	f := cmd.Flags().Lookup(name)
	cv := &choiceValue{Value: f.Value, choices: choices, typeMsg: typeMsg}
	if sl, ok := f.Value.(pflag.SliceValue); ok {
		f.Value = &choiceSliceValue{choiceValue: cv, SliceValue: sl}
	} else {
		f.Value = cv
	}
	_ = cmd.Flags().SetAnnotation(name, "choices", choices)
}

// Parse sequence shared by every order tracked flag, a later stamp marking a later command line token
var flagSetSeq atomic.Uint64

// Flag value stamping the parse sequence on each set, the argv order store_const flags of one dest follow
type orderedValue struct {
	pflag.Value
	at uint64
}

func (v *orderedValue) Set(s string) error {
	if err := v.Value.Set(s); err != nil {
		return err
	}
	v.at = flagSetSeq.Add(1)
	return nil
}

// Stamps parse order on each named flag so the last one given decides the dest they share
func trackOrder(cmd *cobra.Command, names ...string) {
	for _, n := range names {
		f := cmd.Flags().Lookup(n)
		f.Value = &orderedValue{Value: f.Value}
	}
}

// Flag among names given last on the command line, empty when none was given
func (c *CLI) LastGiven(cmd *cobra.Command, names ...string) string {
	last, at := "", uint64(0)
	for _, n := range names {
		f := cmd.Flags().Lookup(n)
		if f == nil || !f.Changed {
			continue
		}
		v, ok := f.Value.(*orderedValue)
		if !ok {
			panic("flag --" + n + " of " + cmd.CommandPath() + " carries no parse order stamp")
		}
		if v.at >= at {
			last, at = n, v.at
		}
	}
	return last
}
