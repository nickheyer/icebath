package gcp

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/proc"
)

const (
	sdkCredentialsFile = "application_default_credentials.json"
	sdkProjectTimeout  = 30 * time.Second
	sdkQuotaWarning    = "Your application has authenticated using end user credentials from Google Cloud SDK without a quota project. You might receive a \"quota exceeded\" or \"API not enabled\" error. See the following page for troubleshooting: https://cloud.google.com/docs/authentication/adc-troubleshooting/user-creds."
)

var credentialKinds = []string{kindUser, kindServiceAccount, kindExternal, kindExternalUser, kindImpersonated, kindGDCH}

func sdkCredentialsPath() (string, error) {
	dir, err := sdkConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sdkCredentialsFile), nil
}

func sdkProject(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, sdkProjectTimeout)
	defer cancel()
	cmd := exec.Command(sdkCommand, "config", "get", "project")
	proc.Hide(cmd)
	res, err := proc.Run(ctx, cmd)
	if err != nil || !res.Exit.Success() {
		return ""
	}
	return strings.TrimSpace(string(res.Stdout))
}

func (c *client) loadFile(path string, scopes []string) (*credentials, string, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, "", &Error{Kind: kindDefault, Message: "file " + path + " was not found", Err: err}
	}
	data, err := readBounded(path)
	if err != nil {
		return nil, "", &Error{Kind: kindDefault, Message: "file " + path + " could not be read", Err: err}
	}
	info, ok := parseDocument(data)
	if !ok {
		return nil, "", &Error{Kind: kindDefault, Message: "file " + path + " is not a valid JSON object"}
	}
	creds, project, err := c.loadInfo(info, path, scopes)
	if err != nil {
		return nil, "", err
	}
	if quota := os.Getenv("GOOGLE_CLOUD_QUOTA_PROJECT"); quota != "" && creds.kind != kindGDCH {
		creds.quotaProject = quota
	}
	if creds.kind == kindUser && creds.quotaProject == "" && info.text("client_id") == cloudSDKClientID {
		slog.Warn(sdkQuotaWarning)
	}
	return creds, project, nil
}

func (c *client) loadInfo(info document, source string, scopes []string) (*credentials, string, error) {
	switch kind := info.text("type"); kind {
	case kindUser:
		creds, err := c.authorizedUser(info)
		return creds, "", err
	case kindServiceAccount:
		creds, err := c.serviceAccount(info, scopes)
		return creds, info.text("project_id"), err
	case kindExternal:
		creds, err := c.externalAccount(info, scopes)
		return creds, "", err
	case kindExternalUser:
		creds, err := c.externalUser(info)
		return creds, "", err
	case kindImpersonated:
		creds, err := c.impersonatedAccount(info, scopes)
		return creds, "", err
	case kindGDCH:
		creds, err := c.gdchAccount(info)
		return creds, info.text("project"), err
	default:
		return nil, "", &Error{Kind: kindDefault, Message: "the file " + source + " does not have a valid type, type is " + kind + ", expected one of " + strings.Join(credentialKinds, ", ")}
	}
}

func (c *client) sdkDefault(ctx context.Context, scopes []string) (*credentials, string, error) {
	path, err := sdkCredentialsPath()
	if err != nil {
		return nil, "", nil
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil, "", nil
	}
	creds, project, err := c.loadFile(path, scopes)
	if err != nil {
		return nil, "", err
	}
	if project == "" {
		project = sdkProject(ctx)
	}
	return creds, project, nil
}

func (c *client) explicitDefault(ctx context.Context, scopes []string) (*credentials, string, error) {
	explicit, ok := os.LookupEnv("GOOGLE_APPLICATION_CREDENTIALS")
	if !ok {
		return nil, "", nil
	}
	if sdkPath, err := sdkCredentialsPath(); err == nil && explicit == sdkPath {
		return c.sdkDefault(ctx, scopes)
	}
	return c.loadFile(explicit, scopes)
}

func (c *client) defaultCredentials(ctx context.Context, scopes []string) (*credentials, string, error) {
	explicitProject, ok := os.LookupEnv("GOOGLE_CLOUD_PROJECT")
	if !ok {
		explicitProject = os.Getenv("GCLOUD_PROJECT")
	}
	creds, project, err := c.explicitDefault(ctx, scopes)
	if err != nil {
		return nil, "", err
	}
	if creds == nil {
		if creds, project, err = c.sdkDefault(ctx, scopes); err != nil {
			return nil, "", err
		}
	}
	if creds == nil {
		creds, project, _ = c.computeDefault(ctx, scopes)
	}
	if creds == nil {
		return nil, "", ErrNoCredentials
	}
	if explicitProject != "" {
		project = explicitProject
	}
	if project == "" && creds.lookup != nil {
		if project, err = creds.lookup(ctx); err != nil {
			return nil, "", err
		}
	}
	if project == "" {
		slog.Warn("No project ID could be determined. Consider running `gcloud config set project` or setting the GOOGLE_CLOUD_PROJECT environment variable")
	}
	return creds, project, nil
}
