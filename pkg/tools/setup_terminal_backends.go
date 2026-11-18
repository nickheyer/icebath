//go:build !icebath_core

package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Adds the wizard steps of the plugins tier backends
func init() {
	terminalBackendSetup["docker"] = (*terminalSetup).backendDocker
	terminalBackendSetup["modal"] = (*terminalSetup).backendModal
	terminalBackendSetup["daytona"] = (*terminalSetup).backendDaytona
	terminalBackendSetup["vercel_sandbox"] = (*terminalSetup).backendVercel
	terminalBackendSetup["ssh"] = (*terminalSetup).backendSSH
}

// Prompts for the Vercel Sandbox image, persistence, resources and access token auth
func (s *terminalSetup) promptVercelSandboxSettings(cfg map[string]any) error {
	setDefault(cfg, "terminal", map[string]any{})
	terminal, err := terminalSection(cfg)
	if err != nil {
		return err
	}
	s.info(nil, "Vercel Sandbox settings:", "  Filesystem persistence uses Vercel snapshots.",
		"  Snapshots restore files only; live processes do not continue after sandbox recreation.")
	currentImage := config.DefaultVercelImage
	if v := terminal["vercel_image"]; pyrepr.Truthy(v) {
		currentImage = pyrepr.Str(v)
	}
	image := pystr.Strip(s.ui.Prompt("  Image (Vercel managed image or VCR repository[:tag])", currentImage, false))
	if image == "" {
		image = currentImage
	}
	terminal["vercel_image"] = image
	if err := s.saveEnv("TERMINAL_VERCEL_IMAGE", image); err != nil {
		return err
	}
	if runtimeValue := terminal["vercel_runtime"]; pyrepr.Truthy(runtimeValue) {
		s.ui.PrintWarning(fmt.Sprintf("terminal.vercel_runtime=%s is deprecated by Vercel and overrides the image; unset it to use the image above.", pyrepr.Repr(runtimeValue)))
	}
	persistLabel := "no"
	if v, ok := terminal["container_persistent"]; !ok || pyrepr.Truthy(v) {
		persistLabel = "yes"
	}
	persist := pystr.Lower(s.ui.Prompt("  Persist filesystem with snapshots? (yes/no)", persistLabel, false))
	terminal["container_persistent"] = slices.Contains([]string{"yes", "true", "y", "1"}, persist)
	resources := []struct {
		key, label string
		def        any
		parse      func(string) (any, bool)
	}{
		{"container_cpu", "  CPU cores", 1, func(text string) (any, bool) {
			f, err := pyint.ParseFloat(text)
			return f, err == nil
		}},
		{"container_memory", "  Memory in MB (5120 = 5GB)", 5120, func(text string) (any, bool) {
			n, ok := pyint.Parse(text)
			if !ok {
				return nil, false
			}
			return pyint.Native(n), true
		}},
	}
	for _, r := range resources {
		current, ok := terminal[r.key]
		if !ok {
			current = r.def
		}
		if parsed, ok := r.parse(s.ui.Prompt(r.label, pyrepr.Str(current), false)); ok {
			terminal[r.key] = parsed
		}
	}
	disk, ok := terminal["container_disk"]
	if !ok {
		disk = 51200
	}
	if !pyrepr.Equal(disk, 0) && !pyrepr.Equal(disk, 51200) {
		s.ui.PrintWarning("Vercel Sandbox does not support custom disk sizing; resetting container_disk to 51200.")
	}
	terminal["container_disk"] = 51200
	s.info(nil, "Vercel authentication:", "  Use a long-lived Vercel access token plus project/team IDs.")
	linked, err := NearestVercelProject("")
	if err != nil {
		return err
	}
	if len(linked) > 0 {
		s.ui.PrintInfo("  Found defaults in nearest .vercel/project.json.")
	}
	if _, err := config.RemoveHomeEnvValue(s.ctx, s.h, "VERCEL_OIDC_TOKEN"); err != nil {
		return err
	}
	fields := []struct {
		label, envVar, linkedKey string
		secret                   bool
	}{
		{"    Vercel access token", "VERCEL_TOKEN", "", true},
		{"    Vercel project ID", "VERCEL_PROJECT_ID", "projectId", false},
		{"    Vercel team ID", "VERCEL_TEAM_ID", "orgId", false},
	}
	for _, f := range fields {
		def, err := s.getEnv(f.envVar)
		if err != nil {
			return err
		}
		if def == "" && f.linkedKey != "" {
			def = linked[f.linkedKey]
		}
		if value := s.ui.Prompt(f.label, def, f.secret); value != "" {
			if err := s.saveEnv(f.envVar, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// Prompts for a secret and saves it to the home .env when non empty, confirming when asked to
func (s *terminalSetup) promptSecretEnv(label, envVar, confirmMsg string) error {
	value := s.ui.Prompt(label, "", true)
	if value == "" {
		return nil
	}
	if err := s.saveEnv(envVar, value); err != nil {
		return err
	}
	if confirmMsg != "" {
		s.ui.PrintSuccess(confirmMsg)
	}
	return nil
}

// True when the variable is already set and the user declines to update it
func (s *terminalSetup) existingSecretKeeps(envVar, label, question string) (bool, error) {
	v, err := s.getEnv(envVar)
	if err != nil || v == "" {
		return false, err
	}
	s.ui.PrintInfo(fmt.Sprintf("  %s: already configured", label))
	return !s.ui.PromptYesNo(question, false), nil
}

// Prints where a binary was found or warns with the install hint
func (s *terminalSetup) reportBinary(found, missing, installHint, foundPrefix string) {
	if found != "" {
		s.ui.PrintInfo(foundPrefix + found)
		return
	}
	s.ui.PrintWarning(missing)
	s.ui.PrintInfo(installHint)
}

// Reports the container runtime, defaults the image and offers the egress firewall
func (s *terminalSetup) backendDocker(cfg map[string]any) error {
	s.ui.PrintSuccess("Terminal backend: Docker / Podman")
	dockerExe, runtimeName := SetupDockerRuntime(s.env)
	prefix := ""
	if dockerExe != "" {
		prefix = runtimeName + " found: "
	}
	s.reportBinary(dockerExe, "Docker or Podman not found in PATH!",
		"Install Docker: https://docs.docker.com/get-docker/ or Podman: https://podman.io/docs/installation", prefix)
	terminal, err := terminalSection(cfg)
	if err != nil {
		return err
	}
	setDefault(terminal, "docker_image", config.DefaultSandboxImage)
	if image, _ := terminal["docker_image"].(string); dockerExe != "" && IsDefaultSandboxImage(image) {
		err := EnsureSandboxImage(s.ctx, dockerExe, image, func(line string, headline bool) {
			if headline {
				s.ui.PrintInfo("  " + line)
			}
		})
		if err != nil {
			s.ui.PrintWarning(fmt.Sprintf("%s; the first Docker sandbox start builds it again", err))
		} else {
			s.ui.PrintSuccess("Sandbox image " + image + " ready")
		}
	}
	s.info(nil, "Docker sandboxes can be protected with the egress credential firewall.",
		"It routes sandbox traffic through iron-proxy so containers receive proxy tokens instead of real API keys.",
		"   Docker only for now; Modal, SSH, Daytona and plugin backends are not wired yet.")
	if !s.ui.PromptYesNo("  Enable egress firewall for Docker sandboxes?", false) {
		s.ui.PrintInfo("Skipping egress firewall. You can enable it later with `icebath egress setup`.")
		return nil
	}
	setDefault(cfg, "proxy", map[string]any{})
	proxy, ok := cfg["proxy"].(map[string]any)
	if !ok {
		return fmt.Errorf("proxy in config is %s, expected a mapping", errchain.Kind(cfg["proxy"]))
	}
	proxy["enabled"] = true
	setDefault(proxy, "enforce_on_docker", true)
	s.ui.PrintSuccess("Egress firewall enabled in config")
	s.ui.PrintInfo("Run `icebath egress setup` then `icebath egress start` to mint tokens and launch the proxy.")
	return nil
}

// Collects the Modal account credentials
func (s *terminalSetup) backendModal(cfg map[string]any) error {
	s.ui.PrintSuccess("Terminal backend: Modal")
	s.ui.PrintInfo("Serverless cloud sandboxes. Each session gets its own container.")
	s.ui.PrintInfo("Requires a Modal account: https://modal.com")
	s.info(nil, "Modal authentication:", "  Get your token at: https://modal.com/settings")
	if keeps, err := s.existingSecretKeeps("MODAL_TOKEN_ID", "Modal token", "  Update Modal credentials?"); err != nil || keeps {
		return err
	}
	if err := s.promptSecretEnv("    Modal Token ID", "MODAL_TOKEN_ID", ""); err != nil {
		return err
	}
	return s.promptSecretEnv("    Modal Token Secret", "MODAL_TOKEN_SECRET", "")
}

// Installs the Daytona SDK, collects the API key and defaults the sandbox image
func (s *terminalSetup) backendDaytona(cfg map[string]any) error {
	s.ui.PrintSuccess("Terminal backend: Daytona")
	s.info("Persistent cloud development environments.",
		"Each session gets a dedicated sandbox with filesystem persistence.",
		"Sign up at: https://daytona.io")
	s.ui.Println()
	existing, err := s.getEnv("DAYTONA_API_KEY")
	if err != nil {
		return err
	}
	keeps, err := s.existingSecretKeeps("DAYTONA_API_KEY", "Daytona API key", "  Update API key?")
	if err != nil {
		return err
	}
	if !keeps {
		confirm := "    Configured"
		if existing != "" {
			confirm = "    Updated"
		}
		if err := s.promptSecretEnv("    Daytona API key", "DAYTONA_API_KEY", confirm); err != nil {
			return err
		}
	}
	terminal, err := terminalSection(cfg)
	if err != nil {
		return err
	}
	setDefault(terminal, "daytona_image", config.DefaultSandboxImage)
	return nil
}

// Announces the Vercel backend and prompts for the sandbox settings
func (s *terminalSetup) backendVercel(cfg map[string]any) error {
	s.ui.PrintSuccess("Terminal backend: Vercel Sandbox")
	s.info("Cloud microVM sandboxes with snapshot-backed filesystem persistence.")
	return s.promptVercelSandboxSettings(cfg)
}

// Collects SSH host, user, port and key, saving the port only when it is not 22, and tests the connection
func (s *terminalSetup) backendSSH(cfg map[string]any) error {
	s.ui.PrintSuccess("Terminal backend: SSH")
	s.ui.PrintInfo("Run commands on a remote machine via SSH.")
	homeDir, err := setupHomeDir()
	if err != nil {
		return err
	}
	fields := []struct{ label, envVar, def string }{
		{"  SSH host (hostname or IP)", "TERMINAL_SSH_HOST", ""},
		{"  SSH user", "TERMINAL_SSH_USER", os.Getenv("USER")},
		{"  SSH port", "TERMINAL_SSH_PORT", "22"},
		{"  SSH private key path", "TERMINAL_SSH_KEY", filepath.Join(homeDir, ".ssh", "id_rsa")},
	}
	values := make([]string, 0, len(fields))
	for _, f := range fields {
		def, err := s.getEnv(f.envVar)
		if err != nil {
			return err
		}
		if def == "" {
			def = f.def
		}
		value := s.ui.Prompt(f.label, def, false)
		values = append(values, value)
		switch {
		case value != "" && (f.envVar != "TERMINAL_SSH_PORT" || value != "22"):
			if err := s.saveEnv(f.envVar, value); err != nil {
				return err
			}
		case f.envVar == "TERMINAL_SSH_PORT" && value == "22":
			if _, err := config.RemoveHomeEnvValue(s.ctx, s.h, f.envVar); err != nil {
				return err
			}
		}
	}
	host, user, port, sshKey := values[0], values[1], values[2], values[3]
	if host == "" || !s.ui.PromptYesNo("  Test SSH connection?", true) {
		return nil
	}
	s.ui.PrintInfo("  Testing connection...")
	code, stderr, err := setupRunSSH(s.ctx, sshProbeArgv(host, user, port, sshKey))
	if err != nil {
		return err
	}
	if code == 0 {
		s.ui.PrintSuccess("  SSH connection successful!")
		return nil
	}
	s.ui.PrintWarning(fmt.Sprintf("  SSH connection failed: %s", pystr.Strip(stderr)))
	s.ui.PrintInfo("  Check your SSH key and host settings.")
	return nil
}
