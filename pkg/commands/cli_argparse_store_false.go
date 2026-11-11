package commands

import "github.com/spf13/cobra"

// Stores false under key when the flag is given and leaves key to its paired flag otherwise, the argparse store_false into a shared dest
func storeFalse(cmd *cobra.Command, name, key string) {
	dest(cmd, name, key)
	_ = cmd.Flags().SetAnnotation(name, "store_false", []string{"true"})
}
