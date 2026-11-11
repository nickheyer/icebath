package updaterecord

// Path of the checkout lock this process holds, false when it holds none
func HeldCheckoutPath() (string, bool) {
	checkout.Lock()
	defer checkout.Unlock()
	if checkout.held == nil {
		return "", false
	}
	return checkout.held.path, true
}

// Stripped stdout of a three second git query in the checkout, empty on any failure
func RunGitQueryText(repoDir string, args ...string) string { return runGitQueryText(repoDir, args...) }
