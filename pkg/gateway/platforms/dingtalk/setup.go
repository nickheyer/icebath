package dingtalk

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// QR device flow of the wizard, the dingtalk_qr_auth name its tests replace
var qrAuthFn = dingtalkQRAuth

// DingTalk wizard of icebath gateway setup, the QR scan or the manual Client ID and Secret
func InteractiveSetup(ctx context.Context) error {
	cli, cfg := gateway.WizardCLI, gateway.WizardConfig
	cli.PrintHeader("DingTalk")
	declined, err := gateway.PlatformSetup.DeclinesReconfigure(ctx, "DingTalk", "Reconfigure DingTalk?", "DINGTALK_CLIENT_ID")
	if err != nil || declined {
		return err
	}
	choices := []string{"QR Code Scan (Recommended, auto-obtain Client ID and Client Secret)", "Manual Input (Client ID and Client Secret)"}
	if gateway.WizardSetup.PromptChoice("Choose setup method", choices, 0) == 0 {
		clientID, clientSecret, ok, err := qrAuthFn(ctx)
		if err != nil {
			return err
		}
		if !ok {
			cli.PrintWarning("QR auth incomplete, falling back to manual input.")
		} else {
			if err := cfg.SaveEnvValue(ctx, "DINGTALK_CLIENT_ID", clientID); err != nil {
				return err
			}
			if err := cfg.SaveEnvValue(ctx, "DINGTALK_CLIENT_SECRET", clientSecret); err != nil {
				return err
			}
			cli.PrintSuccess("DingTalk configured via QR scan!")
			return nil
		}
	}
	return manualCredentialEntry(ctx)
}

// Prompts for the Client ID and optional Client Secret and saves what was given
func manualCredentialEntry(ctx context.Context) error {
	cli, cfg := gateway.WizardCLI, gateway.WizardConfig
	clientID := cli.Prompt("DingTalk Client ID (app key)", "", false)
	if clientID == "" {
		return nil
	}
	if err := cfg.SaveEnvValue(ctx, "DINGTALK_CLIENT_ID", clientID); err != nil {
		return err
	}
	if secret := cli.Prompt("DingTalk Client Secret", "", true); secret != "" {
		if err := cfg.SaveEnvValue(ctx, "DINGTALK_CLIENT_SECRET", secret); err != nil {
			return err
		}
	}
	cli.PrintSuccess("DingTalk credentials saved")
	return nil
}
