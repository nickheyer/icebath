package selfupdate

import (
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

var holderValueFlagsFallback = []string{
	"--profile", "-p", "--config", "--model", "-m", "--provider", "--reasoning", "--toolsets", "-t",
	"--skills", "-s", "--continue", "-c", "--resume", "-r", "--oneshot", "-z", "--in", "--usage-file",
}

var holderFlags struct {
	sync.Mutex
	source func() []string
	cache  map[string]bool
}

func init() {
	updaterecord.SetRuntimeHostMatcher(isRuntimeHost)
	gateway.HolderSubcommand = hermesHolderSubcommand
}

// Installs the top level parser's value consuming option strings, called by the command tree
func SetHolderValueFlags(source func() []string) {
	holderFlags.Lock()
	defer holderFlags.Unlock()
	holderFlags.source = source
}

// Top level flags that consume a value, from the installed parser or the static snapshot, cached per process
func holderValueFlags() map[string]bool {
	holderFlags.Lock()
	defer holderFlags.Unlock()
	if holderFlags.cache != nil {
		return holderFlags.cache
	}
	flags := map[string]bool{}
	if holderFlags.source != nil {
		for _, flag := range append([]string{"--profile", "-p", "--config"}, holderFlags.source()...) {
			flags[flag] = true
		}
	} else {
		for _, flag := range holderValueFlagsFallback {
			flags[flag] = true
		}
	}
	holderFlags.cache = flags
	return flags
}

// Lowercased icebath subcommand a holder command line runs, false when no entry token or subcommand is found
func hermesHolderSubcommand(cmdline string) (string, bool) {
	tokens, err := pyshlex.SplitNonPOSIX(cmdline)
	if err != nil {
		tokens = pystr.SplitWhitespace(cmdline, -1)
	}
	normalized := make([]string, len(tokens))
	for i, token := range tokens {
		normalized[i] = strings.ReplaceAll(pystr.StripChars(token, "\"'"), `\`, "/")
	}
	if gateway.CommandLineRunsInlineSource(normalized) {
		argv, ok := gateway.InlineBootstrapArgv(normalized)
		if !ok {
			return "", false
		}
		tokens = argv
	}
	entry := -1
	for i, token := range tokens {
		low := strings.ReplaceAll(pystr.StripChars(pystr.Lower(token), "\"'"), `\`, "/")
		base := low[strings.LastIndex(low, "/")+1:]
		if (strings.HasSuffix(low, "icebath_cli.main") && i > 0 && tokens[i-1] == "-m") ||
			base == "icebath" || base == "icebath.exe" || strings.HasSuffix(low, "icebath_cli/main.py") {
			entry = i
			break
		}
	}
	if entry < 0 {
		return "", false
	}
	valueFlags := holderValueFlags()
	for i := entry + 1; i < len(tokens); {
		token := tokens[i]
		name, _, hasValue := strings.Cut(token, "=")
		switch {
		case valueFlags[token] || valueFlags[name]:
			if hasValue {
				i++
			} else {
				i += 2
			}
		case strings.HasPrefix(token, "-"):
			i++
		default:
			return pystr.Lower(token), true
		}
	}
	return "", false
}

// Reports whether a command line is a long lived icebath host, a gateway run, serve or dashboard
func isRuntimeHost(cmdline []string) bool {
	line := strings.Join(cmdline, " ")
	if gateway.LooksLikeGatewayCommandLine(line) {
		return true
	}
	sub, ok := hermesHolderSubcommand(line)
	return ok && (sub == "serve" || sub == "dashboard")
}
