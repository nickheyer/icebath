package pm

import (
	"os"
	"testing"
)

// Resolved tree of the running test binary, the checkout the guard protects
func guardCheckoutRoot() string { return resolvePath(RepoRoot()) }

func TestPredicateTrueForLiveCheckoutUnderPytest(t *testing.T) {
	t.Setenv("PYTEST_CURRENT_TEST", "test_checkout_mutation_guards.py::TestPredicate (call)")
	if !liveCheckoutGuard(pytestCaseRunning(), guardCheckoutRoot()) {
		t.Fatal("a pytest case must own the live checkout")
	}
	if !LiveCheckoutUnderTest(guardCheckoutRoot()) {
		t.Fatal("the test run must own the live checkout")
	}
}

func TestPredicateFalseForSandboxedRoot(t *testing.T) {
	t.Setenv("PYTEST_CURRENT_TEST", "test_checkout_mutation_guards.py::TestPredicate (call)")
	root := resolvePath(t.TempDir())
	if liveCheckoutGuard(pytestCaseRunning(), root) || LiveCheckoutUnderTest(root) {
		t.Fatal("a sandboxed root must stay eligible for repair")
	}
}

func TestPredicateFalseOutsidePytest(t *testing.T) {
	t.Setenv("PYTEST_CURRENT_TEST", "")
	os.Unsetenv("PYTEST_CURRENT_TEST")
	if liveCheckoutGuard(pytestCaseRunning(), guardCheckoutRoot()) {
		t.Fatal("outside a test run the live checkout is not owned")
	}
}
