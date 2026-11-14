package azure

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/nickheyer/icebath/internal/proc"
)

const (
	cliOutputLimit      = 1 << 20
	cliNotFound         = "Azure CLI not found on path"
	cliNotLoggedIn      = "Please run 'az login' to set up an account"
	developerNotFound   = "Azure Developer CLI could not be found. Please visit https://aka.ms/azure-dev for installation instructions and then, once installed, authenticate to your Azure account using 'azd auth login'."
	developerNotSigned  = "Please run 'azd auth login' from a command prompt to authenticate before using this credential."
	powershellMissing   = "PowerShell is not installed"
	powershellNoModule  = "Az.Account module >= 2.2.0 is not installed"
	powershellConnect   = `Please run "Connect-AzAccount" to set up account`
	powershellPolicy    = "Execution policy prevented invoking Azure PowerShell"
	powershellFailed    = "Failed to invoke PowerShell."
	powershellNoModuleM = "NO_AZ_ACCOUNT_MODULE"
)

const powershellScript = `$ErrorActionPreference = 'Stop'
[version]$minimumVersion = '2.2.0'

$m = Import-Module Az.Accounts -MinimumVersion $minimumVersion -PassThru -ErrorAction SilentlyContinue

if (! $m) {
    Write-Output NO_AZ_ACCOUNT_MODULE
    exit
}

$params = @{ 'ResourceUrl' = 'RESOURCE_URL'; 'WarningAction' = 'Ignore' }

$tenantId = 'TENANT_ID'
if ($tenantId.Length -gt 0) {
    $params['TenantId'] = $tenantId
}

$useSecureString = $m.Version -ge [version]'2.17.0' -and $m.Version -lt [version]'5.0.0'
if ($useSecureString) {
    $params['AsSecureString'] = $true
}

$token = Get-AzAccessToken @params
$tokenValue = $token.Token
if ($tokenValue -is [System.Security.SecureString]) {
    $ssPtr = [System.Runtime.InteropServices.Marshal]::SecureStringToBSTR($tokenValue)
    try {
        $tokenValue = [System.Runtime.InteropServices.Marshal]::PtrToStringBSTR($ssPtr)
    } finally {
        [System.Runtime.InteropServices.Marshal]::ZeroFreeBSTR($ssPtr)
    }
}
Write-Output "` + "`" + `nazsdk%$($tokenValue)%$($token.ExpiresOn.ToUnixTimeSeconds())` + "`" + `n"
`

var (
	cliTokenField       = regexp.MustCompile(`"accessToken": "(.*?)("|$)`)
	developerTokenField = regexp.MustCompile(`"token": "(.*?)("|$)`)
)

type capped struct{ buf bytes.Buffer }

func (w *capped) Write(p []byte) (int, error) {
	if room := cliOutputLimit - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}

type toolResult struct {
	stdout   string
	stderr   string
	exit     proc.Exit
	missing  bool
	timedOut bool
	err      error
}

func runTool(ctx context.Context, name string, args []string, env []string) toolResult {
	path, err := exec.LookPath(name)
	if err != nil {
		return toolResult{missing: true, err: err}
	}
	dir, err := safeWorkingDir()
	if err != nil {
		return toolResult{err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, processTimeout)
	defer cancel()
	cmd := exec.Command(path, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	proc.Hide(cmd)
	res, err := proc.Run(ctx, cmd)
	out := toolResult{stdout: stdout.buf.String(), stderr: stderr.buf.String(), exit: res.Exit}
	if err != nil {
		out.err = err
		out.timedOut = ctx.Err() != nil
	}
	return out
}

type azureCLI struct{}

func (azureCLI) name() string { return nameCLI }

func (azureCLI) token(ctx context.Context, scope string, inChain bool) (accessToken, error) {
	res := runTool(ctx, "az", []string{"account", "get-access-token", "--output", "json", "--resource", resourceOf(scope)}, []string{"AZURE_CORE_NO_COLOR=true"})
	switch {
	case res.missing:
		return accessToken{}, unavailable(nameCLI, cliNotFound)
	case res.timedOut:
		return accessToken{}, unavailable(nameCLI, "Failed to invoke the Azure CLI")
	case res.err != nil:
		return accessToken{}, &UnavailableError{Credential: nameCLI, Message: "Failed to execute 'az'", Err: res.err}
	case res.exit.Code == 127 || strings.HasPrefix(res.stderr, "'az' is not recognized"):
		return accessToken{}, unavailable(nameCLI, cliNotFound)
	case !res.exit.Success() && (strings.Contains(res.stderr, "az login") || strings.Contains(res.stderr, "az account set")) && !strings.Contains(res.stderr, "AADSTS"):
		return accessToken{}, unavailable(nameCLI, cliNotLoggedIn)
	case !res.exit.Success():
		message := "Failed to invoke Azure CLI"
		if res.stderr != "" {
			message = cliTokenField.ReplaceAllString(res.stderr, "****")
		}
		return accessToken{}, failure(inChain, nameCLI, message)
	}
	var reply struct {
		AccessToken string          `json:"accessToken"`
		ExpiresOn   string          `json:"expiresOn"`
		Epoch       json.RawMessage `json:"expires_on"`
	}
	if json.Unmarshal([]byte(res.stdout), &reply) == nil && reply.AccessToken != "" {
		if len(reply.Epoch) > 0 {
			if epoch, err := strconv.ParseInt(strings.Trim(string(reply.Epoch), `"`), 10, 64); err == nil {
				return accessToken{value: reply.AccessToken, expiresOn: epoch}, nil
			}
		} else if when, err := time.ParseInLocation("2006-01-02 15:04:05", reply.ExpiresOn, time.Local); err == nil {
			return accessToken{value: reply.AccessToken, expiresOn: when.Unix()}, nil
		}
	}
	return accessToken{}, failure(inChain, nameCLI, "Unexpected output from Azure CLI")
}

type developerCLI struct{}

func (developerCLI) name() string { return nameDeveloperCLI }

func (developerCLI) token(ctx context.Context, scope string, inChain bool) (accessToken, error) {
	res := runTool(ctx, "azd", []string{"auth", "token", "--output", "json", "--no-prompt", "--scope", scope}, []string{"NO_COLOR=true"})
	switch {
	case res.missing:
		return accessToken{}, unavailable(nameDeveloperCLI, developerNotFound)
	case res.timedOut:
		return accessToken{}, unavailable(nameDeveloperCLI, "Failed to invoke the Azure Developer CLI")
	case res.err != nil:
		return accessToken{}, &UnavailableError{Credential: nameDeveloperCLI, Message: "Failed to execute 'azd'", Err: res.err}
	case res.exit.Code == 127 || strings.HasPrefix(res.stderr, "'azd' is not recognized"):
		return accessToken{}, unavailable(nameDeveloperCLI, developerNotFound)
	case !res.exit.Success() && strings.Contains(res.stderr, "not logged in, run `azd auth login` to login") && !strings.Contains(res.stderr, "AADSTS"):
		return accessToken{}, unavailable(nameDeveloperCLI, developerNotSigned)
	case !res.exit.Success():
		message := "Failed to invoke Azure Developer CLI"
		if res.stderr != "" {
			message = developerTokenField.ReplaceAllString(res.stderr, "****")
		}
		return accessToken{}, failure(inChain, nameDeveloperCLI, message)
	}
	var reply struct {
		Token     string `json:"token"`
		ExpiresOn string `json:"expiresOn"`
	}
	if json.Unmarshal([]byte(res.stdout), &reply) == nil && reply.Token != "" {
		if when, err := time.Parse("2006-01-02T15:04:05Z", reply.ExpiresOn); err == nil {
			return accessToken{value: reply.Token, expiresOn: when.Unix()}, nil
		}
	}
	return accessToken{}, failure(inChain, nameDeveloperCLI, "Unexpected output from Azure Developer CLI")
}

type powerShell struct{}

func (powerShell) name() string { return namePowerShell }

func encodedCommand(resource string) string {
	script := strings.Replace(strings.Replace(powershellScript, "RESOURCE_URL", resource, 1), "TENANT_ID", "", 1)
	units := utf16.Encode([]rune(script))
	raw := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		raw = binary.LittleEndian.AppendUint16(raw, unit)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func (powerShell) token(ctx context.Context, scope string, inChain bool) (accessToken, error) {
	resource := resourceOf(scope)
	if strings.ContainsAny(resource, "'\r\n") {
		return accessToken{}, failure(inChain, namePowerShell, "scope "+scope+" cannot be passed to PowerShell")
	}
	args := []string{"-NoProfile", "-NonInteractive", "-EncodedCommand", encodedCommand(resource)}
	var res toolResult
	for _, shell := range powerShells {
		if res = runTool(ctx, shell, args, nil); !res.missing {
			break
		}
	}
	switch {
	case res.missing:
		return accessToken{}, unavailable(namePowerShell, powershellMissing)
	case res.err != nil:
		return accessToken{}, &UnavailableError{Credential: namePowerShell, Message: powershellFailed, Err: res.err}
	case res.exit.Success() && strings.Contains(res.stdout, powershellNoModuleM):
		return accessToken{}, unavailable(namePowerShell, powershellNoModule)
	case res.exit.Code == 127 || strings.Contains(res.stderr, "' is not recognized"):
		return accessToken{}, unavailable(namePowerShell, powershellMissing)
	case !res.exit.Success() && strings.Contains(res.stderr, "Run Connect-AzAccount to login"):
		return accessToken{}, unavailable(namePowerShell, powershellConnect)
	case !res.exit.Success() && strings.Contains(res.stderr, "AuthorizationManager check failed"):
		return accessToken{}, unavailable(namePowerShell, powershellPolicy)
	case !res.exit.Success():
		if res.stderr != "" {
			slog.Debug("AzurePowerShellCredential received an error from Azure PowerShell", "exit", res.exit.String())
		}
		return accessToken{}, unavailable(namePowerShell, powershellFailed)
	}
	for _, field := range strings.Fields(res.stdout) {
		if !strings.HasPrefix(field, "azsdk%") {
			continue
		}
		parts := strings.Split(field, "%")
		if len(parts) != 3 {
			break
		}
		epoch, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			break
		}
		return accessToken{value: parts[1], expiresOn: epoch}, nil
	}
	return accessToken{}, failure(inChain, namePowerShell, "Unexpected output from Get-AzAccessToken")
}
