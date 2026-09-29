package supervisor

import (
	"os"
	"testing"

	"github.com/emrul/container-init/internal/execwrap"
)

// TestMain lets the test binary stand in for container-init as the
// spawn wrapper: the supervisor execs /proc/self/exe, which in tests is
// this binary. It can also stand in for a socket-activated service; see
// socketHelperMain.
func TestMain(m *testing.M) {
	execwrap.Main()
	socketHelperMain()
	os.Exit(m.Run())
}
