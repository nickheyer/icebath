package commands

import (
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Profile actions that take a profile name as their next argument
var profileNameActions = []string{"use", "delete", "show", "alias", "rename", "export"}

// One parser level of the completion tree with its option strings, subcommands and help line
type completionNode struct {
	Flags       []string
	Subcommands map[string]*completionNode
	Help        string
}

// Subcommands and option strings of a command tree, aliases and hidden commands left out
func walkCompletion(cmd *cobra.Command) *completionNode {
	node := &completionNode{Flags: []string{"-h", "--help"}, Subcommands: map[string]*completionNode{}}
	fs := cmd.Flags()
	sorted := fs.SortFlags
	fs.SortFlags = false
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Name == "help" || inheritedFlag(cmd, f) {
			return
		}
		if f.Shorthand != "" {
			node.Flags = append(node.Flags, "-"+f.Shorthand)
		}
		node.Flags = append(node.Flags, "--"+f.Name)
		aliases := append([]string{}, f.Annotations["aliases"]...)
		sort.Strings(aliases)
		for _, a := range aliases {
			node.Flags = append(node.Flags, "--"+a)
		}
	})
	fs.SortFlags = sorted
	for _, sub := range cmd.Commands() {
		name := sub.Name()
		if sub.Hidden || strings.HasPrefix(name, "__") {
			continue
		}
		if _, dup := node.Subcommands[name]; dup {
			continue
		}
		child := walkCompletion(sub)
		child.Help = cleanCompletionText(sub.Short)
		node.Subcommands[name] = child
	}
	return node
}

// Reports a flag a parent declares, or one the root consumes before parsing
func inheritedFlag(cmd *cobra.Command, f *pflag.Flag) bool {
	if !cmd.HasParent() {
		return cmd.PersistentFlags().Lookup(f.Name) == f
	}
	return cmd.InheritedFlags().Lookup(f.Name) == f
}

// Strips quotes and backslashes and truncates to sixty characters for shell safety
func cleanCompletionText(text string) string {
	text = strings.NewReplacer("'", "", `"`, "", `\`, "").Replace(text)
	return pystr.Slice(text, 0, 60)
}

// Subcommand names of a completion level in sorted order
func sortedCompletionNames(subcommands map[string]*completionNode) []string {
	names := make([]string, 0, len(subcommands))
	for name := range subcommands {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Bash completion script with top level commands, per command words and profile names
func GenerateBash(root *cobra.Command) string {
	tree := walkCompletion(root).Subcommands
	names := sortedCompletionNames(tree)
	var cases []string
	for _, cmd := range names {
		info := tree[cmd]
		switch {
		case cmd == "profile" && len(info.Subcommands) > 0:
			cases = append(cases, "        profile)\n"+
				"            case \"$prev\" in\n"+
				"                profile)\n"+
				"                    COMPREPLY=($(compgen -W \""+strings.Join(sortedCompletionNames(info.Subcommands), " ")+"\" -- \"$cur\"))\n"+
				"                    return\n"+
				"                    ;;\n"+
				"                "+strings.Join(profileNameActions, "|")+")\n"+
				"                    COMPREPLY=($(compgen -W \"$(_icebath_profiles)\" -- \"$cur\"))\n"+
				"                    return\n"+
				"                    ;;\n"+
				"            esac\n"+
				"            ;;")
		case len(info.Subcommands) > 0 || len(info.Flags) > 0:
			words := info.Flags
			if len(info.Subcommands) > 0 {
				words = sortedCompletionNames(info.Subcommands)
			}
			cases = append(cases, "        "+cmd+")\n"+
				"            COMPREPLY=($(compgen -W \""+strings.Join(words, " ")+"\" -- \"$cur\"))\n"+
				"            return\n"+
				"            ;;")
		}
	}
	return `# Icebath Agent bash completion
# Add to ~/.bashrc:
#   eval "$(icebath completion bash)"

_icebath_profiles() {
    local profiles_dir="$HOME/.icebath/profiles"
    local profiles="default"
    if [ -d "$profiles_dir" ]; then
        for f in "$profiles_dir"/*/; do
            [ -d "$f" ] && profiles="$profiles $(basename "$f")"
        done
    fi
    echo "$profiles"
}

_icebath_completion() {
    local cur prev
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"

    # Complete profile names after -p / --profile
    if [[ "$prev" == "-p" || "$prev" == "--profile" ]]; then
        COMPREPLY=($(compgen -W "$(_icebath_profiles)" -- "$cur"))
        return
    fi

    if [[ $COMP_CWORD -ge 2 ]]; then
        case "${COMP_WORDS[1]}" in
` + strings.Join(cases, "\n") + `
        esac
    fi

    if [[ $COMP_CWORD -eq 1 ]]; then
        COMPREPLY=($(compgen -W "` + strings.Join(names, " ") + `" -- "$cur"))
    fi
}

complete -F _icebath_completion icebath
`
}

// One quoted name and help line per subcommand in sorted order at the given indent
func zshDescribeLines(subcommands map[string]*completionNode, indent string) string {
	var lines []string
	for _, sc := range sortedCompletionNames(subcommands) {
		lines = append(lines, indent+"'"+sc+":"+cleanCompletionText(subcommands[sc].Help)+"'")
	}
	return strings.Join(lines, "\n")
}

// Zsh completion script describing top level commands, their subcommands and profile names
func GenerateZsh(root *cobra.Command) string {
	tree := walkCompletion(root).Subcommands
	var subCases []string
	for _, cmd := range sortedCompletionNames(tree) {
		info := tree[cmd]
		if len(info.Subcommands) == 0 {
			continue
		}
		if cmd == "profile" {
			subCases = append(subCases, "                profile)\n"+
				"                    case ${line[2]} in\n"+
				"                        "+strings.Join(profileNameActions, "|")+")\n"+
				"                            _icebath_profiles\n"+
				"                            ;;\n"+
				"                        *)\n"+
				"                            local -a profile_cmds\n"+
				"                            profile_cmds=(\n"+
				zshDescribeLines(info.Subcommands, strings.Repeat(" ", 24))+"\n"+
				"                            )\n"+
				"                            _describe 'profile command' profile_cmds\n"+
				"                            ;;\n"+
				"                    esac\n"+
				"                    ;;")
			continue
		}
		safe := strings.ReplaceAll(cmd, "-", "_")
		subCases = append(subCases, "                "+cmd+")\n"+
			"                    local -a "+safe+"_cmds\n"+
			"                    "+safe+"_cmds=(\n"+
			zshDescribeLines(info.Subcommands, strings.Repeat(" ", 20))+"\n"+
			"                    )\n"+
			"                    _describe '"+cmd+" command' "+safe+"_cmds\n"+
			"                    ;;")
	}
	return `#compdef icebath
# Icebath Agent zsh completion
# Add to ~/.zshrc:
#   eval "$(icebath completion zsh)"

_icebath_profiles() {
    local -a profiles
    profiles=(default)
    if [[ -d "$HOME/.icebath/profiles" ]]; then
        profiles+=($HOME/.icebath/profiles/*(N/:t))
    fi
    _describe 'profile' profiles
}

_icebath() {
    local context state line
    typeset -A opt_args

    _arguments -C \
        '(-)'{-h,--help}'[Show help and exit]' \
        '(-)'{-V,--version}'[Show version and exit]' \
        '(-)'{-p,--profile}'[Profile name]:profile:_icebath_profiles' \
        '1:command:->commands' \
        '*::arg:->args'

    case $state in
        commands)
            local -a subcmds
            subcmds=(
` + zshDescribeLines(tree, strings.Repeat(" ", 16)) + `
            )
            _describe 'icebath command' subcmds
            ;;
        args)
            case ${line[1]} in
` + strings.Join(subCases, "\n") + `
            esac
            ;;
    esac
}

compdef _icebath icebath
`
}

// Fish completion script with top level commands, their subcommands and profile names
func GenerateFish(root *cobra.Command) string {
	tree := walkCompletion(root).Subcommands
	names := sortedCompletionNames(tree)
	top := strings.Join(names, " ")
	lines := []string{
		"# Icebath Agent fish completion",
		"# Add to your config:",
		"#   icebath completion fish | source",
		"",
		"# Helper: list available profiles",
		"function __icebath_profiles",
		"    echo default",
		"    if test -d $HOME/.icebath/profiles",
		"        for d in $HOME/.icebath/profiles/*/",
		"            basename $d",
		"        end",
		"    end",
		"end",
		"",
		"# Disable file completion by default",
		"complete -c icebath -f",
		"",
		"# Complete profile names after -p / --profile",
		"complete -c icebath -f -s p -l profile -d 'Profile name' -xa '(__icebath_profiles)'",
		"",
		"# Top-level subcommands"}
	for _, cmd := range names {
		lines = append(lines, "complete -c icebath -f -n 'not __fish_seen_subcommand_from "+top+"' -a "+cmd+" -d '"+cleanCompletionText(tree[cmd].Help)+"'")
	}
	lines = append(lines, "", "# Subcommand completions")
	actions := append([]string{}, profileNameActions...)
	sort.Strings(actions)
	for _, cmd := range names {
		info := tree[cmd]
		if len(info.Subcommands) == 0 {
			continue
		}
		lines = append(lines, "# "+cmd)
		for _, sc := range sortedCompletionNames(info.Subcommands) {
			lines = append(lines, "complete -c icebath -f -n '__fish_seen_subcommand_from "+cmd+"' -a "+sc+" -d '"+cleanCompletionText(info.Subcommands[sc].Help)+"'")
		}
		if cmd == "profile" {
			for _, action := range actions {
				lines = append(lines, "complete -c icebath -f -n '__fish_seen_subcommand_from "+action+"; and __fish_seen_subcommand_from profile' -a '(__icebath_profiles)' -d 'Profile name'")
			}
		}
	}
	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

// Prints the zsh or fish script for that shell and the bash script for any other
func (c *CLI) Completion(shell string, w io.Writer) error {
	generate := GenerateBash
	switch shell {
	case "zsh":
		generate = GenerateZsh
	case "fish":
		generate = GenerateFish
	}
	_, err := io.WriteString(w, generate(c.Root)+"\n")
	return err
}
