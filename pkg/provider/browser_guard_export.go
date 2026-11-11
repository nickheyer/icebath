package provider

// Whether this process runs over SSH or another remote session where a local browser cannot open
func IsRemoteSession() bool { return isRemoteSession() }

// Whether a graphical browser can open here, false for text browsers that would take over the TTY
func CanOpenGraphicalBrowser() bool { return canOpenGraphicalBrowser() }
