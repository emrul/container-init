package supervisor

import (
	"testing"

	"github.com/emrul/container-init/internal/userdb"
)

func TestCredentialPlan(t *testing.T) {
	cases := []struct {
		name       string
		euid, egid uint32
		id         userdb.Identity
		wantSwitch bool
		wantErr    bool
	}{
		{"root switches", 0, 0, userdb.Identity{UID: 1000, GID: 1000}, true, false},
		{"root to root still switches", 0, 0, userdb.Identity{UID: 0, GID: 0}, true, false},
		{"non-root, same identity", 1000, 1000, userdb.Identity{UID: 1000, GID: 1000}, false, false},
		{"non-root, other uid", 1000, 1000, userdb.Identity{UID: 1001, GID: 1000}, false, true},
		{"non-root, other gid", 1000, 1000, userdb.Identity{UID: 1000, GID: 29}, false, true},
		{"non-root, wants root", 1000, 1000, userdb.Identity{UID: 0, GID: 0}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sw, err := credentialPlan(tc.euid, tc.egid, tc.id)
			if sw != tc.wantSwitch || (err != nil) != tc.wantErr {
				t.Errorf("credentialPlan = (%v, %v), want (%v, err=%v)", sw, err, tc.wantSwitch, tc.wantErr)
			}
		})
	}
}
