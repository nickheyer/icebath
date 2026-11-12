package commands

import "github.com/spf13/cobra"

// Attaches the hidden install-self verb the install scripts and the setup app hand over to
func (c *CLI) addInstallSelf(root *cobra.Command) {
	cmd := c.newCmd(root, "install-self", "install, update or unhook an icebath release in the icebath home", "")
	cmd.Hidden = true
	sf(cmd, "channel", "", "", "stable (default): the latest published release, then release to release. main: the newest release build of main. Also ICEBATH_INSTALL_CHANNEL; an explicit choice is saved like 'icebath update --set-channel'.")
	sf(cmd, "branch", "", "", "install the newest release built from this branch (default main)")
	sf(cmd, "commit", "", "", "install the release built from this commit")
	sf(cmd, "dir", "", "", "install directory (default ICEBATH_INSTALL_DIR, else <icebath home>/icebath-agent)")
	sf(cmd, "icebath-home", "", "", "icebath home to install into (default ICEBATH_HOME)")
	sf(cmd, "stage", "", "", "run one stage")
	sf(cmd, "binary", "", "", "install this already verified release binary instead of downloading one")
	sf(cmd, "version", "", "", "release version of this binary, the tag the install script verified")
	bf(cmd, "protocol-version", "", "print the stage protocol version")
	bf(cmd, "show-resolved-paths", "", "print the paths this install would use as JSON and exit without touching anything")
	bf(cmd, "from-checkout", "", "publish the checkout's dist/icebath as the icebath command, seed the home and sync skills")
	bf(cmd, "manifest", "", "print the stage list as JSON")
	bf(cmd, "json", "", "print the stage result frame as JSON")
	bf(cmd, "non-interactive", "", "skip stages that need input")
	bf(cmd, "skip-setup", "", "skip stages that need input")
	bf(cmd, "include-desktop", "", "install the desktop app too (products stage)")
	bf(cmd, "verbose", "", "stream every step's output (the default off a terminal and in CI)")
	bf(cmd, "skip-browser", "", "Do not install the browser (Chromium). Remembered by later installs and 'icebath update'; undo with 'icebath pm install chromium'.")
	bf(cmd, "no-playwright", "", "alias of --skip-browser")
	bf(cmd, "skip-computer-use", "", "Do not install the computer-use driver (cua-driver). Remembered the same way; undo with 'icebath pm install cua-driver'.")
	bf(cmd, "update", "", "hand off an update of the existing install, the setup app's --update")
	sf(cmd, "target-app", "", "", "macOS app bundle the update installs the refreshed desktop into")
	bf(cmd, "release-info", "", "print the release tag and commit of this binary as JSON")
	bf(cmd, "uninstall", "", "remove the shell PATH lines, shortcuts and staged installer the install added")
}
