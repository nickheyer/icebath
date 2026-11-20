package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// Points the MFA code prompt at the hosted secret question, the returned func restoring the terminal prompt
func installHostedAWSPrompts(secret func(prompt string) (string, error)) func() {
	awsMFAPromptMu.Lock()
	defer awsMFAPromptMu.Unlock()
	prev := awsMFAPrompt
	awsMFAPrompt = func(ctx context.Context, prompt string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		code, err := secret(prompt)
		if err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return code, nil
	}
	return func() {
		awsMFAPromptMu.Lock()
		awsMFAPrompt = prev
		awsMFAPromptMu.Unlock()
	}
}

// Failure of the credentials Bedrock discovery signs with in the region, nil when they resolve or a bearer token signs
func bedrockDiscoveryCredentialsErr(ctx context.Context, region string) error {
	client, err := bedrockControlClient(ctx, region)
	if err != nil {
		return err
	}
	if client.bearer != "" {
		return nil
	}
	_, err = client.creds.Retrieve(ctx)
	return err
}

// Runs aws sso login on the TUI terminal when discovery credentials lack a valid SSO token, then probes them again
func bedrockHostedSSOLogin(r *ModelFlowRun, ask SetupAsker, region string) {
	ctx := r.ctx()
	var login *awsSSOLoginError
	if !errors.As(bedrockDiscoveryCredentialsErr(ctx, region), &login) {
		return
	}
	aws := hostedProgram("aws")
	if aws == "" {
		fmt.Fprintf(r.Out, "  Profile %s has no valid AWS SSO token and no aws CLI is on the PATH to sign in.\n", login.profile)
		return
	}
	argv := []string{aws, "sso", "login"}
	if login.profile != "default" || os.Getenv("AWS_PROFILE") != "" || os.Getenv("AWS_DEFAULT_PROFILE") != "" {
		argv = append(argv, "--profile", login.profile)
	}
	flowSay(r.Out, fmt.Sprintf("  Profile %s has no valid AWS SSO token.", login.profile), "")
	if askHostedChoice(ask, "AWS SSO sign in", []string{"Sign in with aws sso login", "Continue without signing in"}, 0, "", nil, -1) != 0 {
		fmt.Fprintln(r.Out, "  aws sso login skipped, model discovery runs without a valid SSO token.")
		return
	}
	ran, err := askHostedCommand(ask, argv)
	switch {
	case err != nil:
		fmt.Fprintf(r.Out, "  aws sso login could not start: %v\n", err)
	case !ran:
		fmt.Fprintln(r.Out, "  aws sso login did not finish.")
	default:
		if err := bedrockDiscoveryCredentialsErr(ctx, region); err != nil {
			fmt.Fprintf(r.Out, "  AWS credentials still fail after aws sso login: %v\n", err)
			return
		}
		fmt.Fprintf(r.Out, "  ✓ AWS SSO signed in for profile %s.\n", login.profile)
	}
}
