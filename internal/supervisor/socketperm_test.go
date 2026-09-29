//go:build linux

package supervisor

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/emrul/container-init/unit"
)

// TestSocketOwnership: SocketUser= / SocketGroup= own the socket node.
// SocketUser= alone gives it the user's group, as in systemd. Without
// root the chown is refused, and the socket fails rather than listening
// with the wrong owner.
func TestSocketOwnership(t *testing.T) {
	root := os.Geteuid() == 0
	cases := []struct {
		name, user, group string
		uid, gid          int // want; -1 = container-init's own
	}{
		{"SocketGroup only", "", "1", -1, 1},
		{"SocketUser and SocketGroup", "2", "3", 2, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			address := filepath.Join(t.TempDir(), "sock")
			u := &unit.Unit{Name: "owned.socket", Kind: unit.KindSocket,
				ListenStream: []unit.Listener{{Network: "unix", Address: address}},
				SocketUser:   tc.user, SocketGroup: tc.group, SocketMode: 0o660}
			s := auditSupervisor(t, u)
			ok := s.bindSocket(u)
			defer s.closeSocket(u.Name)
			if !root {
				if ok {
					t.Error("non-root supervisor bound a socket it could not give its owner")
				}
				return
			}
			if !ok {
				t.Fatal("bind failed")
			}
			st, err := os.Stat(address)
			if err != nil {
				t.Fatal(err)
			}
			sys := st.Sys().(*syscall.Stat_t)
			wantUID, wantGID := tc.uid, tc.gid
			if wantUID == -1 {
				wantUID = os.Geteuid()
			}
			if int(sys.Uid) != wantUID || int(sys.Gid) != wantGID {
				t.Errorf("owner = %d:%d, want %d:%d", sys.Uid, sys.Gid, wantUID, wantGID)
			}
			if st.Mode().Perm() != 0o660 {
				t.Errorf("mode = %04o, want 0660", st.Mode().Perm())
			}
		})
	}
}

// TestSocketUserAloneTakesItsGroup: with SocketUser= and no
// SocketGroup=, the node gets the user's primary group from the passwd
// file.
func TestSocketUserAloneTakesItsGroup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown")
	}
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	if err := os.WriteFile(passwd, []byte("svc:x:4242:4343::/nonexistent:/bin/false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	group := filepath.Join(dir, "group")
	if err := os.WriteFile(group, []byte("svc:x:4343:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINER_INIT_PASSWD_FILE", passwd)
	t.Setenv("CONTAINER_INIT_GROUP_FILE", group)
	address := filepath.Join(dir, "sock")
	u := &unit.Unit{Name: "user.socket", Kind: unit.KindSocket,
		ListenStream: []unit.Listener{{Network: "unix", Address: address}}, SocketUser: "svc"}
	s := auditSupervisor(t, u)
	if !s.bindSocket(u) {
		t.Fatal("bind failed")
	}
	defer s.closeSocket(u.Name)
	st, err := os.Stat(address)
	if err != nil {
		t.Fatal(err)
	}
	sys := st.Sys().(*syscall.Stat_t)
	if sys.Uid != 4242 || sys.Gid != 4343 {
		t.Errorf("owner = %d:%d, want 4242:4343", sys.Uid, sys.Gid)
	}
	if st.Mode().Perm() != 0o666 {
		t.Errorf("mode = %04o, want systemd's default 0666", st.Mode().Perm())
	}
}
