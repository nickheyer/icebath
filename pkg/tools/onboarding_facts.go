package tools

import "github.com/nickheyer/icebath/pkg/hostinfo"

var onboardingOSNames = map[string]string{"darwin": "macos", "win32": "windows"}

// Facts reader of every run start, replaced by a case as the hermes cases patch read_facts
var ReadOnboardingFacts = readOnboardingFacts

// Measures the safe facts a run is built from, the desktop locale preferred over the OS one
func readOnboardingFacts(env *Env, factsSource, locale string) OnboardingFacts {
	requireOnboardingHooks()
	host := hostinfo.HostSummary()
	osFamily, _ := host.Machine["os_family"].(string)
	osName, ok := onboardingOSNames[osFamily]
	if !ok {
		osName = "linux"
	}
	arch, _ := host.Machine["native_arch"].(string)
	source := "local"
	if factsSource == "remote" {
		source = "remote"
	}
	if locale == "" {
		locale = host.Locale
	}
	return OnboardingFacts{OS: osName, Arch: arch, MachineKind: host.MachineKind, NvidiaPlatform: host.NvidiaPlatform,
		FactsSource: source, Locale: locale, Plugins: OnboardingPluginNames(env), Tours: desktopUserEnabled(env, "in_app_tours", true), FullName: host.FullName}
}

// The OS account's real name, empty when the OS has none to give
func hostSummaryFullName() string { return hostinfo.HostSummary().FullName }
