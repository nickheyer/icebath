package pm

import (
	"os"
	"testing"
)

// Reports root as the checkout this binary runs from while go test or a pytest case drives the process
func LiveCheckoutUnderTest(root string) bool {
	return liveCheckoutGuard(testing.Testing() || pytestCaseRunning(), root)
}

// Reports a pytest case in progress that spawned this process
func pytestCaseRunning() bool {
	_, set := os.LookupEnv("PYTEST_CURRENT_TEST")
	return set
}

// Reports root as the tree of the running binary when underTest holds, any other root staying eligible for repair
func liveCheckoutGuard(underTest bool, root string) bool {
	return underTest && root == resolvePath(binaryTree())
}
