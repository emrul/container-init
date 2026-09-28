package supervisor

import (
	"os"
	"testing"

	"github.com/emrul/container-init/internal/execwrap"
)

// TestMain lets the test binary stand in for container-init as the
// spawn wrapper: the supervisor execs /proc/self/exe, which in tests is
// this binary.
func TestMain(m *testing.M) {
	execwrap.Main()
	os.Exit(m.Run())
}
