// Package userdb resolves User= / Group= directive values to numeric
// uid/gid plus supplementary group lists by reading /etc/passwd and
// /etc/group directly. We do not shell out to id(1) / getent(1) — the
// extra fork-exec is gratuitous when /etc/passwd is a few KiB of text
// and we already need to handle the lookup synchronously inside the
// supervisor's pre-exec path.
//
// Both spellings (numeric "1500" and named "alice") are accepted. A
// numeric form skips the file lookup; a named form must resolve.
package userdb

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// File paths are exposed via package-level vars so unit tests can
// redirect lookups to fixture files. Production code reads
// /etc/passwd / /etc/group only.
var (
	passwdPathOverride = "/etc/passwd"
	groupPathOverride  = "/etc/group"
)

// Identity is the resolved view of a User= directive plus optional
// Group=. SupplementaryGroups is populated from /etc/group entries
// that name the user as a member.
type Identity struct {
	Username string
	UID      uint32
	GID      uint32
	Home     string
	// SupplementaryGroups holds the gids of every group whose member
	// list includes Username, excluding the primary GID.
	SupplementaryGroups []uint32
}

// Resolve looks up user / group / homeOverride and returns the
// resolved Identity. Pass empty group to default to the user's
// primary GID. Pass empty homeOverride to use the passwd home dir.
//
// User and group accept either a numeric id ("1500") or a name
// ("alice"). Numeric ids are taken as-is; named entries must exist
// in /etc/passwd / /etc/group.
func Resolve(user, group, homeOverride string) (Identity, error) {
	if user == "" {
		return Identity{}, fmt.Errorf("userdb: empty user")
	}
	id := Identity{}
	if uid, err := strconv.ParseUint(user, 10, 32); err == nil {
		id.UID = uint32(uid)
		if e, err := lookupPasswdByUID(id.UID); err == nil {
			id.Username = e.name
			id.GID = e.gid
			id.Home = e.home
		} else {
			// Numeric uid, no /etc/passwd entry — accept and continue
			// with synthetic name; supplementary group lookup needs
			// a username so leave SupplementaryGroups empty.
			id.Username = user
		}
	} else {
		e, err := lookupPasswdByName(user)
		if err != nil {
			return Identity{}, fmt.Errorf("userdb: user %q: %w", user, err)
		}
		id.Username = e.name
		id.UID = e.uid
		id.GID = e.gid
		id.Home = e.home
	}

	if group != "" {
		if gid, err := strconv.ParseUint(group, 10, 32); err == nil {
			id.GID = uint32(gid)
		} else {
			ge, err := lookupGroupByName(group)
			if err != nil {
				return Identity{}, fmt.Errorf("userdb: group %q: %w", group, err)
			}
			id.GID = ge.gid
		}
	}

	if homeOverride != "" {
		id.Home = homeOverride
	}

	if id.Username != "" {
		extras, err := supplementaryGroups(id.Username, id.GID)
		if err != nil {
			return Identity{}, fmt.Errorf("userdb: supplementary groups for %q: %w", id.Username, err)
		}
		id.SupplementaryGroups = extras
	}
	return id, nil
}

type passwdEntry struct {
	name string
	uid  uint32
	gid  uint32
	home string
}

type groupEntry struct {
	name    string
	gid     uint32
	members []string
}

func lookupPasswdByName(name string) (passwdEntry, error) {
	return scanPasswd(func(e passwdEntry) bool { return e.name == name })
}

func lookupPasswdByUID(uid uint32) (passwdEntry, error) {
	return scanPasswd(func(e passwdEntry) bool { return e.uid == uid })
}

func scanPasswd(match func(passwdEntry) bool) (passwdEntry, error) {
	f, err := os.Open(passwdPathOverride)
	if err != nil {
		return passwdEntry{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8*1024), 64*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 7 {
			continue
		}
		uid, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			continue
		}
		gid, err := strconv.ParseUint(fields[3], 10, 32)
		if err != nil {
			continue
		}
		e := passwdEntry{name: fields[0], uid: uint32(uid), gid: uint32(gid), home: fields[5]}
		if match(e) {
			return e, nil
		}
	}
	if err := sc.Err(); err != nil {
		return passwdEntry{}, err
	}
	return passwdEntry{}, fmt.Errorf("not found")
}

func lookupGroupByName(name string) (groupEntry, error) {
	return scanGroup(func(e groupEntry) bool { return e.name == name })
}

func scanGroup(match func(groupEntry) bool) (groupEntry, error) {
	f, err := os.Open(groupPathOverride)
	if err != nil {
		return groupEntry{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8*1024), 64*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 4 {
			continue
		}
		gid, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			continue
		}
		var members []string
		if fields[3] != "" {
			members = strings.Split(fields[3], ",")
		}
		e := groupEntry{name: fields[0], gid: uint32(gid), members: members}
		if match(e) {
			return e, nil
		}
	}
	if err := sc.Err(); err != nil {
		return groupEntry{}, err
	}
	return groupEntry{}, fmt.Errorf("not found")
}

// supplementaryGroups walks /etc/group and returns the gids of every
// group whose member list includes username, excluding primaryGID.
func supplementaryGroups(username string, primaryGID uint32) ([]uint32, error) {
	f, err := os.Open(groupPathOverride)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []uint32
	seen := map[uint32]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8*1024), 64*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 4 {
			continue
		}
		gid64, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			continue
		}
		gid := uint32(gid64)
		if gid == primaryGID || seen[gid] {
			continue
		}
		if fields[3] == "" {
			continue
		}
		for _, m := range strings.Split(fields[3], ",") {
			if m == username {
				out = append(out, gid)
				seen[gid] = true
				break
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
