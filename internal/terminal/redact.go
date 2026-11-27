package terminal

import (
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/internal/tools/redact"
)

// Output of a command run for the profile as the model or a notice gets it: the values the vault filled for the profile always taken out,
// its secrets masked when the profile hides them, or always when forced, the values of secret assignments too when the command dumps the
// environment or prints a file that holds secrets
func Redact(cfg *config.Config, output, command string, force bool) string {
	output = redact.Filled(cfg.Profile(), output)
	if output == "" || !force && !cfg.GetBool(toolsconfig.RedactSecrets) {
		return output
	}
	return redact.Command(output, command, secretFile(cfg))
}

// Command line as a result or a notice names it, the secrets it holds masked when the profile hides them, or always when forced
func RedactCommand(cfg *config.Config, command string, force bool) string {
	command = redact.Filled(cfg.Profile(), command)
	if command == "" || !force && !cfg.GetBool(toolsconfig.RedactSecrets) {
		return command
	}
	return redact.Text(command, false, redact.Ends)
}

// Text of a failure a call reports, the secrets it quotes masked whatever the profile says, the values of secret assignments too
func RedactError(text string) string {
	return redact.Text(text, true, redact.Ends)
}

// Whether a path a command prints holds secrets: a project env file, a shell startup file, or icebath's own config.yaml, one in a .icebath
// folder or in the profile's or the settings' folder
func secretFile(cfg *config.Config) func(path string) bool {
	roots := []string{resolved(cfg.ProfileDir(cfg.Profile())), resolved(cfg.Dir())}
	under := func(path, root string) bool {
		p := resolved(path)
		return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
	}
	return func(path string) bool {
		return redact.SecretFile(path, roots, under)
	}
}
