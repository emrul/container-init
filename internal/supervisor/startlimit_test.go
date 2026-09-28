package supervisor

import (
	"testing"
	"time"

	"github.com/emrul/container-init/unit"
)

func TestStartLimiter(t *testing.T) {
	t0 := time.Unix(1000, 0)
	l := newStartLimiter(&unit.Unit{StartLimitBurst: 3, StartLimitIntervalSec: 10 * time.Second})
	for i := 0; i < 3; i++ {
		if !l.allow(t0.Add(time.Duration(i) * time.Second)) {
			t.Fatalf("start %d refused within burst", i+1)
		}
	}
	if l.allow(t0.Add(9 * time.Second)) {
		t.Error("4th start within the interval allowed")
	}
	// The window opened at the first start; once it has elapsed a
	// new one admits another burst.
	for i := 0; i < 3; i++ {
		if !l.allow(t0.Add(10*time.Second + time.Duration(i)*time.Millisecond)) {
			t.Fatalf("start %d in the new window refused", i+1)
		}
	}
	if l.allow(t0.Add(11 * time.Second)) {
		t.Error("4th start in the new window allowed")
	}
}

func TestStartLimiterDisabled(t *testing.T) {
	for _, u := range []*unit.Unit{
		{},
		{StartLimitBurst: 3},
		{StartLimitIntervalSec: time.Second},
	} {
		l := newStartLimiter(u)
		for i := 0; i < 100; i++ {
			if !l.allow(time.Unix(1000, 0)) {
				t.Fatalf("burst=%d interval=%v: start %d refused with no limit",
					u.StartLimitBurst, u.StartLimitIntervalSec, i+1)
			}
		}
	}
}

// TestStartLimitSharedAcrossPaths: every launch path calls allowStart,
// so two sockets activating one service, or its own loop plus
// OnFailure=, draw on one budget rather than one each.
func TestStartLimitSharedAcrossPaths(t *testing.T) {
	svc := &unit.Unit{Name: "svc.service", StartLimitBurst: 1, StartLimitIntervalSec: time.Minute}
	s := newDepsSupervisor(t, svc)
	if !s.allowStart(svc) {
		t.Fatal("first start refused")
	}
	if s.allowStart(svc) {
		t.Error("second start from another path allowed past StartLimitBurst=1")
	}
}
