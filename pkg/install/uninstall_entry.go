package install

// Apps and Features key and identity WinGet matches against the manifest's AppsAndFeaturesEntries
const (
	UninstallEntryKey       = `HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\nickheyer.icebath`
	uninstallEntryKey       = `Software\Microsoft\Windows\CurrentVersion\Uninstall\nickheyer.icebath`
	uninstallEntryName      = "icebath"
	uninstallEntryPublisher = "Nicholas W. Heyer"
)
