package selfupdate

import "context"

// True for the system scope when this process is not root
func NeedsSudo(scope string) bool { return needsSudo(scope) }

// True when this user elevates without a prompt, by the blanket probe then the targeted one
func SudoNoninteractiveOk(ctx context.Context, targetedProbe []string) bool {
	return sudoNoninteractiveOk(ctx, targetedProbe)
}
