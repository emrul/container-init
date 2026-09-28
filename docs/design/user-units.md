# Design: per-user units and `systemctl --user`

Status: proposal
Date: 2026-09-28

## Summary

Let users of a workspace image (anyone with a `/home/<user>`) define
their own services with ordinary systemd user units, have them start at
container boot, and manage them with the real `systemctl --user
start|stop|restart|status|enable|disable|daemon-reload`.

Nothing about it is container-init-specific from the user's side:

- units live in `~/.config/systemd/user/`,
- they are enabled with `systemctl --user enable` (a symlink in
  `default.target.wants/`),
- start-at-boot is switched on per user with systemd's lingering
  marker, `/var/lib/systemd/linger/<user>`,
- `systemctl --user` reaches container-init over the same private
  socket it uses to reach a real `systemd --user`,
  `$XDG_RUNTIME_DIR/systemd/private`.

A unit that works here should work unchanged under real systemd, and
the reverse holds for the supported directive subset.

The hard requirement: **a user unit always runs as its owner and can
never gain anything the user does not already have.** PID 1 runs as
root, so every place where root reads, parses, expands, or acts on
something the user controls is a potential privilege escalation. Most
of this document is about closing those places off.

## Goals

- User units, enabled the systemd way, start at boot for opted-in users.
- `systemctl --user` works for `start`, `stop`, `restart`,
  `try-restart`, `status`, `is-active`, `is-failed`, `is-enabled`,
  `list-units`, `list-unit-files`, `enable`, `disable`,
  `daemon-reload`, `reset-failed`, `kill`.
- User units run as the user, only as the user, and are isolated from
  the system unit graph and from other users.
- A broken or hostile user unit cannot stop the container from booting,
  take it down, or delay its shutdown past a fixed budget.

## Non-goals (v1)

- `.socket`, `.timer`, `.path`, `.target` user units. `.service` only.
- Template units (`foo@.service` / `foo@bar.service`).
- A journal. `systemctl status` shows state, not log lines (see
  [Logs](#logs)).
- `systemctl --user` for users who are not opted in (there is no login
  to start a manager on demand).
- `systemd-run --user` running real transient units. The existing
  systemd1 shim's stub behaviour is kept for that.
- `/etc/systemd/user` and `/usr/lib/systemd/user`. Distros ship
  enablement symlinks there (pipewire, wireplumber, gpg-agent sockets)
  and loading them would start a desktop session stack nobody asked
  for. They can be added later behind a flag.
- `systemctl` for the system scope. The runtime-control work below makes
  that possible later but it is not part of this design.

## User-facing behaviour

### Opting a user in

A user gets a user manager at boot when either is true:

1. `/var/lib/systemd/linger/<username>` exists (what `loginctl
   enable-linger` creates; an image can `touch` it at build time), or
2. the user is named in `--user-managers=alice,bob` (or
   `CONTAINER_INIT_USER_MANAGERS`, for images that cannot change argv).

`--user-managers=none` disables the feature regardless of linger files,
so an image author can turn it off completely.

Users are resolved through `internal/userdb` at boot. A user with no
passwd entry, no home directory, or UID 0 is skipped with a log line.
Root never gets a user manager: root's "user units" would run as root,
and system units already cover that.

### Writing and enabling a unit

```ini
# ~/.config/systemd/user/notes-sync.service
[Unit]
Description=Sync notes

[Service]
ExecStart=%h/bin/notes-sync --watch %h/notes
Restart=on-failure

[Install]
WantedBy=default.target
```

```
$ systemctl --user daemon-reload
$ systemctl --user enable --now notes-sync
$ systemctl --user status notes-sync
```

At boot, every unit linked from `~/.config/systemd/user/default.target.wants/`
starts. Units present but not enabled are loaded (so `start` and
`status` work) but not started. Other `*.target.wants/` directories
(`graphical-session.target.wants/` and so on) are ignored with one
info line, since those targets are never reached here.

### What the user sees in the container log

Output from user units is tagged with the manager, as system units are
tagged with their name:

```
[user@1000/notes-sync.service] watching /home/alice/notes
```

## Architecture

```
                    PID 1 (container-init, root)
 ┌──────────────────────────────────────────────────────────────┐
 │ pid1.Dispatcher (single reaper, shared)                      │
 │                                                              │
 │ Supervisor[system]        ── /etc/container-init/units + .d  │
 │                                                              │
 │ UserManager[alice]                                           │
 │   Supervisor[user@1000]   ── units loaded by helper as alice │
 │   Policy[user@1000]       ── forced identity, env, limits    │
 │   ControlServer           ── /run/user/1000/systemd/private  │
 │                                                              │
 │ UserManager[bob] ...                                         │
 └──────────────────────────────────────────────────────────────┘
          ▲                          │ spawn via exec-helper
          │ D-Bus (p2p, EXTERNAL)    ▼ (already running as alice)
   systemctl --user (alice)     alice's service processes
```

The model copies systemd's: one manager per user, each with its own
unit graph. A user manager is a second `Supervisor` instance with a
`Scope` and a `Policy`, sharing PID 1's dispatcher and cgroup manager.
Separate instances give isolation by construction: a user unit cannot
name, order against, override, or be reached from a system unit,
because it is in a different graph.

### New and changed packages

| Package | Change |
|---|---|
| `unit/` | `Scope` in `Options` (system / user). User scope: forbidden-directive policy, user specifiers, env lookup from the user's environment, not PID 1's. |
| `internal/supervisor/` | Runtime per-unit control: `StartUnit`, `StopUnit`, `RestartUnit`, `KillUnit`, `ResetFailed`, `Reload`, state and job events. Replaces one-shot ready channels. Accepts a `Policy`. |
| `internal/usermgr/` (new) | Discovery (linger, flag), runtime dir setup, helper invocation, one supervisor + control server per user, ordered shutdown. |
| `internal/sdbus/` (new) | Server side of D-Bus p2p over a Unix socket: SASL EXTERNAL server handshake, peer-credential checks, handing the connection to godbus. |
| `internal/systemctl/` (new) | The `org.freedesktop.systemd1` object model a real `systemctl` expects: Manager, Unit, Service, Job objects backed by a supervisor. |
| `internal/systemd1shim/` | When the shim runs on a user bus for a user that has a manager, delegate the unit methods to that manager's object model. `StartTransientUnit` stays a stub. |
| `cmd/container-init/` | `--user-managers` flag; hidden `__user-helper` subcommand (see [The user-side helper](#the-user-side-helper)); starts user managers after the system boot pass. |
| `third_party/godbus/` | Patch: start the connection's read loop without running client auth, so an already-authenticated server-side connection can be served. Recorded in `PATCHES.md`. |

## Security model

The trust boundary is the user. Everything under the user's home, the
user's runtime directory, and every byte that arrives on the control
socket is attacker-controlled from PID 1's point of view.

### Threats and safeguards

| # | Threat | Safeguard |
|---|---|---|
| T1 | Unit sets `User=root`, `User=bob`, `Group=wheel` | Hard load error in user scope. Identity is never read from the unit; it is fixed by the manager. See [Forced identity](#forced-identity). |
| T2 | Unit omits `User=` and so runs as root (today's default for system units) | The user-scope spawn path has no "no identity" case: credentials are always the manager's user, and a unit that would start without them is a bug that fails closed. |
| T3 | Root reads a user-controlled path: symlinked unit file → `/etc/shadow`, `EnvironmentFile=/root/.secrets`, `ConditionPathExists=/root/x` probing | PID 1 never opens a user-controlled path. All reads happen in a helper process already running as the user. See [The user-side helper](#the-user-side-helper). |
| T4 | `${VAR}` expansion or inherited environment leaks PID 1's environment (image secrets, tokens) into the user's process | User units never inherit PID 1's environment. No load-time `${VAR}` expansion in user scope; command lines expand at exec time against the service's final environment, which contains nothing from PID 1 except the image author's passthrough allowlist. `ConditionEnvironment=` is evaluated against the user manager's environment. See [Environment](#environment). |
| T5 | `ExitContainerOnFailure=true` takes the container down | Hard load error in user scope. |
| T6 | `Before=web.service` / `After=` / `Requires=` / `OnFailure=` reach into the system graph (today `resolveBefore` mutates the *target* unit) | Separate supervisor per scope; names resolve only within the user's graph, so a system unit's name is just an unknown name there. Unknown references are kept, not dropped, and follow systemd's rules for a not-found unit: see [Missing dependencies](#missing-dependencies). |
| T7 | Unit named like a system unit replaces it | Separate namespace (`user@1000/web.service`). No override across scopes. |
| T8 | `.socket` unit binds a privileged port or a root-owned path through PID 1 | `.socket` not accepted in user scope in v1. A later version must bind in the helper or restrict to ports ≥ 1024 and paths under the user's runtime dir. |
| T9 | `PIDFile=` / `Type=forking` points PID 1 at another user's or root's process, which it then signals | `PIDFile=` and `Type=forking` rejected in user scope in v1. PID 1 never signals a bare PID in user scope; every signal goes through a stable process handle or `cgroup.kill`. See [Signalling](#signalling). |
| T10 | `systemctl kill --signal=…` or `KillUnit` used to signal processes outside the user's units, including by winning a PID-reuse race between a uid check and `kill(2)` | Signals only reach processes whose membership in the unit's cgroup was checked *after* a pidfd was opened for them, and are sent through that pidfd. A PID reused in between yields a pidfd for the new process, which fails the check. See [Signalling](#signalling). |
| T11 | Another user connects to alice's control socket | Socket `0600`, owned by alice, in `/run/user/<uid>/systemd/` (`0700`, owned by alice). On accept, `SO_PEERCRED` uid must be alice's uid or 0; anything else is closed before the handshake. The EXTERNAL identity, if supplied, must match the peer uid. |
| T12 | A control connection tries to act on another user's units or system units | Each socket is bound to exactly one manager. There is no way to name another scope; the object model only contains that manager's units. |
| T13 | Unit restarts in a tight loop, fills logs, forks without bound | User units get systemd's default start limit (`StartLimitBurst=5`, `StartLimitIntervalSec=10s`) when they set neither, unlike system units. Optional per-user `pids.max` / `memory.max` on the `user@<uid>` cgroup (`--user-pids-max`, `--user-memory-max`). A per-user cap on loaded units (default 128) and on unit file size (default 64 KiB). |
| T14 | User unit ignores SIGTERM and delays container shutdown | User managers stop first, inside a fixed budget (default 5s, `--user-stop-timeout`), and are then killed through their cgroup. A unit's `TimeoutStopSec=` is capped by that budget. |
| T15 | Malformed unit files break boot, even under `--strict-units` | User-scope load problems are always warnings for that unit, never fatal to the container. `--strict-units` applies to image-owned units only. |
| T16 | `daemon-reload` or D-Bus call flood as a DoS against PID 1 | Per-connection message size cap, per-manager cap on concurrent connections (default 16) and pending jobs, `daemon-reload` rate limit (e.g. 1/s). Parsing runs in the helper, not PID 1. |
| T17 | Symlink or ownership tricks on `/run/user/<uid>` so PID 1 chowns or writes somewhere else | Runtime dir creation uses `openat` with `O_NOFOLLOW` from `/run/user`. An existing path must be a real directory owned by the user; otherwise the manager is not started and a line is logged. PID 1 never follows a link under a user-owned directory. |
| T18 | Log injection: user output that looks like PID 1 or system-unit lines | Every line from a user unit is prefixed `[user@<uid>/<unit>] ` by the existing line-prefix writer; lines are capped (e.g. 16 KiB) and control characters other than tab are escaped. |
| T19 | Enabling a unit that points outside the unit directory (`systemctl link` style) | Wants symlinks are resolved by name only, within `~/.config/systemd/user/`. A link whose target lies outside that directory is ignored with a warning. Enable/disable write the symlinks through the helper, as the user. |
| T20 | Service forks before PID 1 moves it into its cgroup; the early children stay in PID 1's cgroup, escape `cgroup.kill`, per-user limits and shutdown | The child is created inside its cgroup with `clone3(CLONE_INTO_CGROUP)` (Go: `SysProcAttr.UseCgroupFD`), so no user instruction runs outside it. A boot-time probe must show creation, atomic spawn, pidfd signalling and `cgroup.kill` all working (in practice Linux ≥ 5.14); otherwise user managers do not start. See [Cgroup containment](#cgroup-containment). |
| T21 | Helper or exec wrapper blocks forever: unit file or `EnvironmentFile=` is a FIFO, a device, or on a hung FUSE/NFS mount; PID 1 stalls in boot, reload, or shutdown | Non-regular files are refused without blocking (`O_NONBLOCK` open, then `fstat` must be `S_ISREG`). Every helper run has a deadline and is killed on expiry or shutdown; PID 1 holds no lock while waiting on one; helper concurrency is bounded. See [Bounding the helper](#bounding-the-helper). |

### Forced identity

In user scope the loader treats these as hard errors, which stop the
unit from loading (the unit then shows `LoadState=bad-setting`, which
is also what systemd reports):

- `User=`, `Group=` (systemd's user manager also cannot switch user)
- `ExitContainerOnFailure=`
- `PIDFile=`, `Type=forking` (v1)
- any `.socket` unit (v1)

The spawn path for user scope:

- sets `SysProcAttr.Credential` to the user's uid, primary gid and
  supplementary groups from `userdb`, always;
- sets `HOME`, `USER`, `LOGNAME`, `SHELL` from the passwd entry,
  `XDG_RUNTIME_DIR=/run/user/<uid>`, and
  `DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/<uid>/bus` when that
  socket exists;
- creates the process directly inside cgroup
  `container-init/user@<uid>/<unit>` (see
  [Cgroup containment](#cgroup-containment));
- obtains a pidfd for the main process at creation
  (`SysProcAttr.PidFD`) and keeps it for the life of the process;
- sets `no_new_privs` (`PR_SET_NO_NEW_PRIVS`) so a setuid binary run
  from a user unit cannot regain root. This is stricter than systemd,
  where `sudo` from a user unit works; note it in the README, and allow
  an image-level opt-out (`--user-allow-new-privs`) for images that rely
  on `sudo` in user services.

The same credential struct is used for the helper, the service process
and every `ExecStartPre=` / `ExecStop=` process when those are
implemented, so there is one place identity is decided.

When container-init itself runs without root (the container runs with
`--user`), only a manager for container-init's own uid is possible.
`credentialPlan` already enforces this; the other safeguards still
apply.

### Cgroup containment

Today `spawnAndWait` starts the child and then writes its PID to the
unit's `cgroup.procs`. Moving a process does not move children it has
already forked, so a hostile service can fork in that window and leave
descendants in PID 1's own cgroup, beyond `cgroup.kill`, the per-user
`pids.max` / `memory.max`, and the shutdown sweep. The existing comment
calls the window "empirically empty"; for user code it has to be empty
by construction.

User-scope spawns therefore:

1. create the leaf cgroup `container-init/user@<uid>/<unit>` (under a
   parent carrying the per-user limits) and open it as a directory fd;
2. start the child with `SysProcAttr.UseCgroupFD = true` and
   `CgroupFD` set to that fd, so Go uses `clone3(CLONE_INTO_CGROUP)`
   and the child exists only inside the leaf cgroup from its first
   instruction;
3. never migrate a user process after the fact.

This applies to the `__user-helper` and `__user-exec` processes as
well, since they run as the user too (helpers go in
`user@<uid>/helper`).

**Fail closed, after proving teardown works.** Containment needs
more than atomic placement: shutdown and stop depend on `cgroup.kill`
(Linux 5.14), signalling depends on pidfds (`pidfd_open` 5.3,
`pidfd_send_signal` 5.1), and `CLONE_INTO_CGROUP` arrived in 5.7. A
5.10 kernel has the last three but not the first, so a probe that only
spawns a child would pass while shutdown containment is missing.
Before any user manager starts, PID 1 runs a probe that exercises the
whole lifecycle, once, at boot:

1. create `container-init/user-probe/` and, if `--user-pids-max` or
   `--user-memory-max` is set, enable the `pids` / `memory` controllers
   for it and write the limits;
2. spawn a probe child (`/proc/self/exe __probe`, which forks one
   grandchild and then blocks) with `UseCgroupFD` and `PidFD` set;
3. confirm from `/proc/<pid>/cgroup` that the child is in the probe
   leaf, and that `cgroup.procs` lists both the child and its
   grandchild;
4. send signal 0 through the pidfd (`pidfd_send_signal`);
5. write `1` to `cgroup.kill`, then wait (bounded, 2s) for the pidfd to
   become readable and for `cgroup.events` to report `populated 0`,
   which shows the grandchild died too;
6. `rmdir` the probe cgroup.

Any failure, for any reason (`ENOSYS`, `EINVAL`, `EPERM` or `EACCES`
from seccomp, AppArmor or a read-only cgroup mount, a missing
`cgroup.kill` file, a controller that cannot be enabled, or a timeout),
means no user manager starts. PID 1 logs one line naming the failed
step and the error, once per opted-in user, and system units boot as
usual. The result is cached for the life of PID 1. There is no PGID
fallback for user scope: process groups are under the user's control
(`setsid`, `setpgid`) and cannot contain them.

The user never gets write access to any cgroup file: the subtree is
not delegated, so a user process cannot move itself out of its leaf,
and cgroup v2's migration rules would not let it write into an
ancestor's `cgroup.procs` anyway.

### Signalling

A PID is not a stable name: between reading it and calling `kill(2)`,
the process can exit and the PID be reused by another user's or root's
process, and PID 1 would signal that one with root's authority.
Checking the uid first does not help. So in user scope:

- **Main process.** Signalled only through the pidfd obtained at clone
  time (`pidfd_send_signal`). The pidfd keeps referring to the same
  process after it exits and is reaped, so a late signal gets `ESRCH`,
  never a stranger.
- **SIGKILL to the whole unit** (stop timeout, shutdown,
  `systemctl kill -s KILL`). `cgroup.kill`, which the kernel applies
  atomically to the cgroup's members; no PIDs involved.
- **Any other signal to the whole unit** (`KillUnit` with
  `--kill-whom=all`, or `KillSignal=` on stop). For each PID in
  `cgroup.procs`: `pidfd_open(pid)`, then read `/proc/<pid>/cgroup`
  and require it to be the unit's leaf, then `pidfd_send_signal`
  through that fd. If the PID was reused before `pidfd_open`, the pidfd
  names the new process and the membership check rejects it; after
  the check the pidfd cannot be redirected, and the user cannot move a
  process out of the leaf. Repeat the scan until a pass finds no new
  members (bounded to a few passes), which catches children forked
  during the scan, as systemd does. Optionally freeze the leaf
  (`cgroup.freeze`) for the scan so forks cannot race it.
- **`--kill-whom=main`** uses the main-process pidfd;
  `--kill-whom=control` is not supported (no control processes in v1).

No path in user scope takes a PID from user input: not from a file, not
from D-Bus arguments. The system supervisor keeps its current behaviour
in this design, but moving it to the same primitives is a cheap
follow-up once they exist.

### Environment

User units start from a clean environment, as under `systemd --user`:

1. a fixed base: `PATH=/usr/local/bin:/usr/bin:/bin`, `LANG=C.UTF-8`
   unless overridden below;
2. variables named in `--user-env-passthrough=LANG,TZ,...` copied from
   PID 1's environment (an image-author allowlist; nothing by default);
3. `~/.config/environment.d/*.conf`, read by the helper, in systemd's
   format and order;
4. the identity variables above (always win over 1–3);
5. the unit's `Environment=` lines, in order;
6. the unit's `EnvironmentFile=` files, in order, read by the exec
   wrapper as the user. As in `systemd.exec(5)`, settings from these
   files override `Environment=`, and a later file overrides an earlier
   one.

Layers 1–4 are the user manager's environment; layers 5–6 are
per-service and are only known at exec time, because environment files
are read then.

**Expansion follows systemd, not today's load-time expansion.**
container-init currently expands `${VAR}` and `${VAR:-default}` in every
directive at load time against PID 1's environment. That is a
container-init extension (it is what makes `User=${APP_USER:-app}`
work), and in user scope it is both a leak (T4) and wrong: with
`Environment=PORT=8080` and `ExecStart=/usr/bin/server --port ${PORT}`,
load-time expansion would not see `PORT`. In user scope:

- At load, only specifiers (`%h`, `%u`, …) are expanded, in every
  directive, as systemd does. `${…}` is left as written.
- `ExecStart=` (and `ExecStartPre=` / `ExecStop=` when implemented)
  expands at exec time, in the `__user-exec` wrapper, against the
  final environment (layers 1–6): `${VAR}` substitutes as one argument
  and `$VAR` as whitespace-split words, per `systemd.service(5)`.
  `${VAR:-default}` is not systemd syntax and is not supported in user
  scope; a unit that uses it gets a load warning.
- `Environment=` values are not variable-expanded, as in systemd.
- `WorkingDirectory=` and other path directives take specifiers only.

PID 1 passes the wrapper the manager environment, the `Environment=`
entries, the `EnvironmentFile=` list and the unexpanded argv over a
pipe; the wrapper merges, expands and `execve`s.

Specifiers add the user-manager set: `%h`
(home), `%u` (user name), `%U` (uid), `%g`, `%G`, `%t`
(`XDG_RUNTIME_DIR`), `%S` / `%C` / `%L` (`~/.local/state`, `~/.cache`,
`~/.local/state/log`, following `systemd.unit(5)` for user managers).

### The user-side helper

PID 1 must not open files the user controls, so a helper does it.
container-init re-executes itself (`/proc/self/exe __user-helper`) with
the user's credentials and `no_new_privs`, and talks to it over a pipe.
The helper is short-lived and has two jobs:

- **load**: read `~/.config/systemd/user/*.service`, the
  `default.target.wants/` links, and `~/.config/environment.d/*.conf`;
  return file contents as length-prefixed JSON, with size and count
  caps. It does not evaluate conditions; those belong to activation
  (see [Conditions](#conditions)).
- **enable / disable**: create or remove the `default.target.wants/`
  symlink for a named unit, returning the change list `systemctl`
  prints.

Every start goes through an exec wrapper in the same binary
(`__user-exec`), which PID 1 spawns already running as the user and
already inside the unit's cgroup. The wrapper:

1. evaluates the unit's path conditions (see [Conditions](#conditions));
2. reads `EnvironmentFile=` and builds the final environment;
3. `chdir`s to `WorkingDirectory=` (default: the user's home, as for
   `systemd --user`);
4. expands the command line (see [Environment](#environment));
5. `execve`s the real `ExecStart=`.

It reports which step it reached on a close-on-exec status pipe, so PID
1 can tell "condition not met", "environment file missing", and "exec
failed" apart from the service's own exit status, and learns that the
`execve` succeeded when the pipe closes with no message.

**Nothing user-controlled runs in the parent's spawn path.** Go's
`cmd.Start` does not return until the child has either exec'd or
failed, and everything the child does before `execve` (credential
switch, `chdir` to `cmd.Dir`, cgroup placement) happens inside that
wait. `Dispatcher.Spawn` holds the dispatcher lock across `cmd.Start`
(`internal/pid1/reaper.go:89`), and the reaper needs that lock to
deliver exit statuses. So if `cmd.Dir` were a user's directory on a
hung FUSE or NFS mount, `chdir` would block, `cmd.Start` would block,
and PID 1 would stop reaping and spawning for every unit, system units
included. The start deadline would not help, because it starts counting
after `Spawn` returns.

So PID 1 spawns the wrapper with a fixed, trusted setup: `cmd.Path` is
`/proc/self/exe`, `cmd.Dir` is `/`, and the argv and environment carry
no user paths (the unit description goes over the pipe). The only
thing left between fork and exec is kernel work on root-owned objects.
The wrapper does the `chdir` itself (step 3), after `Spawn` has
returned, so a hang there is an ordinary slow start: it counts against
`TimeoutStartSec=`, and on expiry the unit's cgroup is killed. The
same rule applies to the load helper, which starts in `/` and opens the
user's paths itself.

The kernel still checks `WorkingDirectory=` as the user, because the
wrapper already runs with the user's credentials when it calls
`chdir`.

**The dispatcher lock stays; `Start` becomes bounded instead.**
Holding the dispatcher lock across `cmd.Start` is what makes
registration safe: the reaper cannot `wait4` a child until `Spawn` has
recorded which consumer it belongs to, so every exit status reaches the
spawn that created that process. Dropping the lock and buffering the
statuses of unregistered PIDs would break that. Once a process is
reaped its PID can be reused, so two concurrent spawns could claim each
other's buffered status by number, and a spawn that never returns would
keep the "buffer every unknown exit" window open for good, collecting
the statuses of every orphan reparented to PID 1 in the meantime.

The real problem is only that `Start` can block for as long as the
child's pre-exec steps take, and those steps include `chdir` and the
`execve` of a path that may sit on a hung mount. The fix is to make sure
no path outside PID 1's control is touched between fork and exec, for
every spawn:

- system units get the same treatment as user units: PID 1 spawns
  `/proc/self/exe __exec` with `cmd.Dir = "/"`, and the wrapper does
  `chdir(WorkingDirectory=)` and the `execve` of `ExecStart=` itself,
  after `Spawn` has returned and released the lock (the wrapper runs as
  root, or as the unit's `User=`, as the service would);
- the only things between fork and exec are then credential setup,
  `CLONE_INTO_CGROUP`, and the exec of PID 1's own binary: kernel work
  on objects PID 1 owns, which cannot wait on a user's or an image's
  filesystem.

`Spawn` keeps its contract (register the PID under the same lock as the
fork) and the reaper keeps reaping everything with `wait4(-1)`.

Tests for this, in the system-supervisor fix:

- a unit whose `WorkingDirectory=` blocks (a test hook in the wrapper
  that blocks before `chdir`, standing in for a hung mount) while other
  units spawn, exit and are reaped normally, and while stop still
  kills it;
- many concurrent spawns of commands that exit immediately, each
  checking it received its own exit status (distinct exit codes);
- orphan churn: a unit that continuously double-forks short-lived
  grandchildren, which PID 1 reaps as unknown PIDs, while other units
  spawn and exit, with every registered exit delivered to the right
  consumer;
- PID reuse: exhaust a small `pid_max` range in a PID namespace (the
  test runs as PID 1 of its own namespace) so PIDs recycle while spawns
  and orphan reaping continue, with the same delivery checks.

PID 1 parses what the load helper returns with the same `unit/` parser
and applies the user-scope policy itself. The helper's output is
untrusted input: a compromised helper is still only the user, and the
policy check is in PID 1.

### Bounding the helper

Size caps limit what the helper returns, not how long it takes. A unit
file can be a FIFO, whose `open` blocks until a writer appears; a path
can sit on a FUSE or NFS mount that never answers. Both would stall PID
1 if it waited without limit. So:

- **Only regular files.** The helper and the exec wrapper open every
  unit file, drop-in, `environment.d` file and `EnvironmentFile=` with
  `O_RDONLY | O_NONBLOCK | O_CLOEXEC`, which does not block on a FIFO,
  then `fstat` the fd and refuse anything that is not `S_ISREG`, with a
  warning naming the file. Directory listings use `Lstat` and skip
  entries that are not regular files or symlinks. Symlinked unit files
  are allowed, as in systemd, but the check is on the opened fd, so a
  link to a FIFO or device is refused the same way. Reads stop at the
  size cap.
- **Deadlines.** A load or enable/disable run has a deadline (default
  10s, `--user-helper-timeout`). On expiry PID 1 kills it with
  `cgroup.kill` on the helper's cgroup, which also takes out anything it
  forked, and treats the run as failed: at boot the manager starts with
  no units and logs why; on `daemon-reload` the previous unit set is
  kept and the D-Bus call returns an error. Filesystem calls that hang
  in the kernel can leave the helper unkillable in `D` state, which is
  why PID 1 only ever waits on it through a pipe with a deadline and
  never on the syscall.
- **The exec wrapper** runs inside the unit's own start: its time counts
  against `TimeoutStartSec=` (systemd's default 90s when unset), and a
  timeout fails the start job with result `timeout` and kills the unit
  cgroup, as for any start that hangs.
- **Cancellation.** Helper runs take a context derived from the manager's
  lifetime. Shutdown and manager teardown cancel it, which kills the
  helper; shutdown never waits for a helper to finish.
- **Bounded concurrency, charged until reaped.** At most one
  load/reload/enable/disable helper per manager at a time (a second
  request waits behind it or is coalesced) and a global limit across
  managers (default 4), so a boot with many opted-in users cannot fork
  a helper per user at once.

  Helpers are in one of two states, and they are counted separately:

  - **Running** (within their deadline). These hold the concurrency
    slots. A request that finds its manager's slot or all global slots
    held by running helpers *waits* in a FIFO queue; the time spent
    queued counts toward the request's own deadline. This is the normal
    case: a fifth opted-in user at boot waits a moment for one of the
    four loads ahead of it, it does not start empty.
  - **Stuck** (past their deadline, killed, not yet reaped). A helper
    in an uninterruptible sleep on a hung mount survives `cgroup.kill`
    until the kernel lets it go. On timeout the run fails and the
    caller gets its answer, and the helper moves from its concurrency
    slot to the stuck set. It stays charged there until it is
    *reaped*, never released on timeout, so repeated `daemon-reload`s
    cannot pile up blocked processes.

  Admission rejects a request immediately, rather than queueing it,
  only because of stuck helpers: when the requesting manager already
  has a stuck helper (at most one per manager), or when the global
  stuck count has reached its cap (default 8). The error names the
  stuck helper and what it was reading. `daemon-reload`, `enable` and
  `disable` return it as a D-Bus error that `systemctl` prints; a
  manager starting at boot starts with no units and logs it. So the
  number of helper processes is bounded by running slots plus the stuck
  cap, and healthy load never causes a rejection. The same accounting
  covers `__probe`.

  Exec wrappers are not in these slots: they are the unit's own
  processes. A unit whose cgroup is still populated after `cgroup.kill`
  (a wrapper or service stuck in `D` state) stays `deactivating` and
  refuses new start jobs until the cgroup empties, so a restart loop
  cannot stack stuck processes either. The per-user `pids.max` bounds
  the total in any case.
- **No locks across helper I/O.** Load and reload build the new unit set
  in a local structure with no supervisor lock held, then take the lock
  only to swap it in. Starts, stops and D-Bus status calls keep working
  while a reload's helper runs.

## Runtime control in the supervisor

Today `Supervisor.Run` starts everything once; the only stop is the
global reverse shutdown, and readiness is a one-shot channel per unit.
`systemctl start|stop|restart` needs per-unit control:

- **Per-unit state machine** with systemd's names, so `status` and
  `is-active` can report them directly: `ActiveState` in
  `inactive | activating | active | deactivating | failed`, `SubState`
  (`dead | start | running | exited | stop-sigterm | failed |
  auto-restart`), `Result` (`success | exit-code | signal | timeout |
  start-limit-hit | resources`), plus timestamps, `MainPID`,
  `ExecMainCode`/`ExecMainStatus`, `NRestarts`.
- **Readiness as a state, not a closed channel.** Replace
  `ready map[string]chan struct{}` with the state above plus a
  broadcast (a channel swapped on each transition under `s.mu`).
  `waitDeps` waits for a dependency to be `active`, or to be settled
  as failed/inactive, re-arming after a restart. This removes the
  one-shot limitation for system units too.
- **Per-unit stop.** Each running unit gets its own stop channel,
  closed by `StopUnit` or by global shutdown; `spawnAndWait` selects on
  both. `StopUnit` sends `KillSignal=` (default SIGTERM), waits up to
  `TimeoutStopSec=` (capped for user scope), then `cgroup.kill`.
  `TimeoutStopSec=` and `KillSignal=` are parsed today but not used;
  this is where they get used.
- **Jobs and transactions.** Each `StartUnit`/`StopUnit`/`RestartUnit`
  builds a transaction and completes with systemd's result strings
  (`done`, `failed`, `timeout`, `dependency`, `skipped`, `canceled`).
  Mode `replace` is the only mode implemented; others are accepted and
  treated as `replace`, except `isolate`, which is refused. See
  [Transactions and ordering](#transactions-and-ordering).
- **Stopping a unit stops its `Requires=` dependents**, as in systemd
  (`BindsTo=` / `PartOf=` are out of scope).
- **Reload** (`daemon-reload`) re-runs the load helper, diffs the unit
  set, adds new units, marks removed ones (`LoadState=not-found`) and
  leaves running processes alone until they stop, as systemd does.
- **Events** go to both the trace tracer and a subscriber list the
  D-Bus layer uses to emit `JobNew` / `JobRemoved` / `UnitNew` /
  `UnitRemoved` / `PropertiesChanged`.

### Transactions and ordering

systemd keeps two things apart that container-init currently merges.
Requirement dependencies (`Requires=`, `Wants=`) decide *which* units
are pulled into a transaction. Ordering dependencies (`After=`,
`Before=`) decide *in what order* the jobs in it run. `Requires=`
without `After=` starts both units in parallel. Today `topoSort` adds
an ordering edge for every `Requires=` and `waitDeps` waits on
`Requires=` as well as `After=`, so `A Requires=B` with `A Before=B`,
which is valid in systemd (B needs A, A must be up first), becomes a
cycle and the unit set is rejected.

The runtime model:

1. **Inclusion.** A start job for A pulls in a start job for every unit
   in A's `Requires=` and `Wants=`, transitively. Stop jobs pull in stop
   jobs for units that `Requires=` the stopped unit. Boot is a start
   transaction for the enabled set (in user scope,
   `default.target.wants/`).
2. **Ordering.** Only `After=` and `Before=` edges order jobs, and they
   apply against every job *installed in the manager*, not just the
   ones from the same transaction. For two units joined by an ordering
   edge (`A After=B`, or equivalently `B Before=A`) that both have a
   job installed, which job waits depends on both jobs' types, as in
   systemd's job ordering comparator:

   | A's job | B's job | Who waits |
   |---|---|---|
   | start | start | A waits for B (the edge's direction) |
   | stop | stop | B waits for A (reversed) |
   | start | stop | A waits for B: the stop finishes first |
   | stop | start | B waits for A: the stop finishes first |

   A stop always goes first in a mixed pair, whichever way the edge
   points, so a mixed pair can never wait on each other. A job is
   runnable when no installed job it must wait for under this table is
   pending or running. So after `systemctl start --no-block B` (a slow
   oneshot) and a separate `systemctl start A` with `A After=B`, A's job
   waits for B's. Edges to units with no installed job have no effect:
   `A After=B` with B inactive and not being started does not start or
   wait for B. When a job completes, the scheduler re-checks the jobs
   waiting on it. A cycle is only among ordering edges; it is reported
   with the units in it and fails the transaction that would create it
   (systemd tries to break cycles by dropping `Wants=` jobs; v1 does
   not).
   **One job per unit.** A unit has at most one installed job. A new
   job of the same type merges into the installed one (the caller
   waits for the existing job). A conflicting one in mode `replace`
   (start while a stop is pending, or stop while a start is pending)
   cancels the installed job, which completes with `canceled`, and
   takes its place; a job that is already running is not interrupted
   mid-exec but finishes its current step first (a stop replacing a
   running start kills the unit once the start has spawned it).
3. **Failure propagation.** If B's start job fails and A `Requires=B`,
   A's job fails with `dependency` only if A is also ordered after B.
   Without ordering, A has already been started in parallel and stays
   running, as in systemd. `Wants=` never propagates failure. A
   not-found unit in `Requires=` fails the whole transaction before
   anything starts (see below).

Regression cases for Phase 3, each checked against systemd's behaviour
in the Phase 0 environment:

- `A Requires=B`, `A Before=B`: loads; A starts first, then B.
- `A Requires=B` with no ordering: A and B start concurrently; B
  failing does not stop A from starting.
- `A Requires=B`, `A After=B`, B fails: A's job ends `dependency`.
- `A Wants=B`, `A After=B`, B fails: A starts.
- `A After=B` with nothing pulling B in: A starts without waiting.
- `A After=B`, `B After=A`, both in one transaction: rejected as a
  cycle naming A and B.
- `systemctl start --no-block B` (B a slow oneshot), then
  `systemctl start A` with only `A After=B`: A's job waits until B's
  finishes.
- Mixed start/stop, both edge directions, each with both jobs
  installed at once and neither waiting on the other in a loop:
  - `A After=B`, start A + stop B: stop B runs first, then start A.
  - `A After=B`, stop A + start B: stop A runs first, then start B.
  - `A Before=B`, start A + stop B: stop B runs first, then start A.
  - `A Before=B`, stop A + start B: stop A runs first, then start B.
- Same-type pairs in both directions: with `A After=B`, start A + start
  B runs B first; stop A + stop B runs A first.
- `systemctl stop A` while A's start job is still waiting on an ordering
  dependency: the start job completes `canceled`, the stop job runs.
- `systemctl start A` twice in quick succession: one start, both callers
  get the same job result.

(This is also a bug in the system supervisor today. It is one of the
existing-behaviour fixes listed under [Open questions](#open-questions).)

### Missing dependencies

Today `topoSort` and `waitDeps` skip any name that is not loaded, for
every dependency type. With `Requires=setup.service` and no
`setup.service`, the dependent starts anyway, which is the opposite of
what `Requires=` promises. In user scope this also covers a user naming
a system unit, since system units are not in the user's graph.

User scope keeps every reference and treats an unknown name as a unit
with `LoadState=not-found`, as systemd does:

| Reference to a missing unit | Effect |
|---|---|
| `Requires=` | The start job fails with result `dependency` and the unit does not start. `systemctl start` reports it; `status` shows the missing unit. A later `daemon-reload` that adds the unit makes the next start succeed. |
| `Wants=` | Ignored: the start proceeds. |
| `After=` / `Before=` | Ordering only, and there is nothing to order against: no effect. |
| `OnFailure=` | Nothing to start when the unit fails; logged once. |

Each missing reference gets one load warning naming both units, so a
typo shows up at boot rather than as a silently weaker dependency.
Not-found units appear in `list-units --all`, as in systemd.

(The system supervisor has the same silent skip for `Requires=`. Fixing
it there is a behaviour change for existing images and is left out of
this design; see [Open questions](#open-questions).)

### Conditions

Today conditions are evaluated once, when units are loaded. systemd
evaluates them when a start job actually runs: after the unit's
`After=` dependencies have started and just before the first
`ExecStartPre=`/`ExecStart=`. That matters for the common pattern of one
unit creating a file another unit's `ConditionPathExists=` tests, and
for a user who creates the file and then runs `systemctl --user start`.

User scope evaluates conditions on every start, at that point:

- `ConditionUser=` and `ConditionEnvironment=` need no file access and
  are evaluated in PID 1, against the user's identity and the manager
  environment.
- `ConditionPathExists=` and `ConditionPathExistsGlob=` are evaluated by
  the `__user-exec` wrapper as step 1, as the user, inside the unit's
  cgroup and under `TimeoutStartSec=`. If one fails, the wrapper
  reports it on the status pipe and exits before running anything
  else.

A failed condition is not a failure, as in systemd: the unit stays
`inactive`, `ConditionResult=no` and the condition text are exposed for
`status`, the job completes with result `done`, `Requires=` dependents
still start, and `OnFailure=` does not fire. Whether a skipped start
counts against the start limit is to be matched to systemd in Phase 0.

The system supervisor's load-time evaluation is unchanged by this
design.

The system supervisor gets the same machinery, so a future system-scope
`systemctl` is a D-Bus listener away. System units behave as today at
boot.

## The control socket and `systemctl --user`

### Transport

`systemctl --user` connects directly to
`$XDG_RUNTIME_DIR/systemd/private` (a peer-to-peer D-Bus connection, no
bus daemon) before it tries the session bus. Serving that socket means
`systemctl --user` works in images with no `dbus-daemon`, which is most
headless workspaces. It also gives the authorization boundary for free:
one socket per user, owned by that user.

`systemctl` checks the peer credentials of the private socket and
accepts a server running as root or as the calling user, so a root PID 1
is acceptable to it. (Verify in Phase 0.)

### Server handshake

godbus only implements the client side, so `internal/sdbus` does the
server half of SASL itself, before handing the connection over:

1. `accept`, read `SO_PEERCRED`; close unless the uid is the manager's
   or 0.
2. Read the leading NUL, then line-based SASL. Accept
   `AUTH EXTERNAL <hex-uid>` (must equal the peer uid) and `AUTH
   EXTERNAL` followed by `DATA`, in either order, including pipelined
   input (sd-bus sends its whole auth conversation before reading
   replies). Reply `OK <server-guid>`. Answer `NEGOTIATE_UNIX_FD` with
   `ERROR` (no fd passing needed, which keeps the transport generic).
   Reject every other mechanism.
3. On `BEGIN`, wrap the socket in a godbus `Conn` and start its read
   loop (the godbus patch), export the object model, and serve.

Limits: handshake deadline 5s, per-connection message size cap, max
connections per manager.

### Object model

`internal/systemctl` exposes what the supported verbs read. The exact
method set is to be confirmed by the Phase 0 capture; the expected set:

- `/org/freedesktop/systemd1`, `org.freedesktop.systemd1.Manager`:
  `StartUnit`, `StopUnit`, `RestartUnit`, `TryRestartUnit`,
  `ReloadOrRestartUnit`, `KillUnit`, `ResetFailedUnit`,
  `ResetFailed`, `GetUnit`, `LoadUnit`, `ListUnits`,
  `ListUnitsFiltered`, `ListUnitsByPatterns`, `ListUnitsByNames`,
  `ListUnitFiles`, `ListUnitFilesByPatterns`, `GetUnitFileState`,
  `EnableUnitFiles`, `EnableUnitFilesWithFlags`, `DisableUnitFiles`,
  `DisableUnitFilesWithFlags`, `Reload`, `Subscribe`, `Unsubscribe`;
  properties `Version`, `SystemState`, `Environment`, `NNames`,
  `NFailedUnits`. Signals `JobNew`, `JobRemoved`, `UnitNew`,
  `UnitRemoved`, `Reloading`.
- `/org/freedesktop/systemd1/unit/<escaped>`:
  `org.freedesktop.systemd1.Unit` and `…Service` properties via
  `org.freedesktop.DBus.Properties` (`Get`, `GetAll`,
  `PropertiesChanged`); `LoadUnit` on an unknown name returns an object
  with `LoadState=not-found`, which is how `status` prints "could not
  be found".
- `/org/freedesktop/systemd1/job/<id>`: `org.freedesktop.systemd1.Job`
  (`Id`, `Unit`, `JobType`, `State`).
- `org.freedesktop.DBus.Introspectable` and `org.freedesktop.DBus.Peer`
  on all of the above.

Anything else returns `org.freedesktop.DBus.Error.UnknownMethod` or
`org.freedesktop.systemd1.NotSupported`; `Exit`, `PowerOff`, `Reboot`,
`Halt`, `KExec`, `SwitchRoot`, `SetEnvironment`,
`UnsetEnvironment`, `StartTransientUnit` are refused explicitly.
`Exit` in particular must never reach PID 1's lifecycle.

`systemctl` reads properties with lenient maps, so we can return the
subset we track; properties we do not track are simply absent.

### Session bus interaction

If the image also runs the systemd1 shim on the user's session bus (the
Ptyxis case), the shim, when a user manager for that uid exists,
delegates the unit methods to the same object model, so a client that
goes through the bus sees the same units as `systemctl`. The shim keeps
answering `StartTransientUnit` with its stub, which is what makes
`systemd-run --user --scope` succeed today.

### Logs

There is no journal, so `systemctl --user status` shows state, PIDs,
the command line, and exit codes but no recent log lines. Output
continues to go to the container's stdout with the `[user@uid/unit]`
prefix. A later option could also write each user unit's output to
`~/.local/state/container-init/<unit>.log` (written by the exec
wrapper, as the user), so users can `tail` it. Not in v1.

## Boot and shutdown ordering

- **Boot**: the system supervisor starts as today. User managers start
  once every boot-started system unit has settled (active, failed, or
  skipped), mirroring `user@.service` coming up late in systemd boot.
  Before starting, each manager creates `/run/user/<uid>` (`0700`,
  owned by the user) if it does not exist, and
  `/run/user/<uid>/systemd/`. The control socket is listening before any
  user unit starts, so `ExecStart=` scripts can call `systemctl --user`.
- **Shutdown**: user managers stop first, in parallel with each other,
  each in reverse dependency order within the user budget (T14); then
  the system supervisor's reverse shutdown runs as today. Control
  sockets close first, so no new jobs start during shutdown.
- **A user manager that fails** (helper cannot run, runtime dir unsafe)
  logs and stays down. It never affects system units or other users.

## Build plan

Each phase is shippable on its own and keeps existing behaviour for
images that do not opt in.

**Phase 0: protocol spike (no product code).**
Run real systemd user managers (Debian 12 / systemd 252, Ubuntu 24.04 /
systemd 255, Fedora latest) in privileged containers. For each supported
verb, capture the D-Bus traffic `systemctl --user` sends on the private
socket (strace the socket, or point `systemctl` at the bus and use
`busctl --user monitor`). Output: the exact method and property set per
verb and systemd version, the SASL byte sequence sd-bus sends, how
sender matching of `JobRemoved` works on a p2p connection, whether
`systemctl --user` needs anything from `/run/systemd/system` or
`sd_booted()`, and what `status` prints with no journal. These become
golden fixtures for Phase 4.

**Phase 1: user-scope loading and policy (`unit/`).**
`Options.Scope`, forbidden directives, user specifiers, no load-time
`${…}` expansion in user scope, not-found placeholders for unknown
references, default start limit for user scope. Table tests for every
entry in the threat table that the loader owns (T1, T4, T5, T6, T7, T8,
T9, T15).

**Phase 2: helper, identity and boot start (`internal/usermgr`,
`cmd/container-init`).**
Discovery (linger + flag), runtime dir setup, `__user-helper` load,
`__user-exec` wrapper (conditions, environment files, exec-time
expansion, status pipe), helper deadlines, cancellation and concurrency
limits, `clone3(CLONE_INTO_CGROUP)` spawning with the boot-time probe
and fail-closed behaviour, pidfd capture, a user-scope `Supervisor`
instance started at boot, `no_new_privs`, ordered shutdown. At the end
of this phase "start at boot" works with no `systemctl`. Tests, in the
Linux CI job with a non-root test uid: symlink-to-root-file and
env-leak regressions (T3, T4, T17); a service that forks immediately
and has every descendant in its leaf cgroup (T20); a FIFO unit file and
a FIFO `EnvironmentFile=` that neither block boot nor shutdown (T21);
a `WorkingDirectory=` that blocks in `chdir` (a FUSE test mount that
never answers) while other units keep spawning and being reaped;
repeated `daemon-reload` against a helper stuck in `D` state that is
rejected once slots are exhausted rather than accumulating processes;
the probe failing closed when `cgroup.kill` is absent or blocked;
environment precedence and `${VAR}`/`$VAR` expansion against
`systemd.exec(5)` examples; a condition satisfied by a prerequisite
unit at run time.

**Phase 3: runtime control in the supervisor.**
State machine, broadcast readiness, per-unit stop, jobs, reload with
no lock held across helper I/O, activation-time conditions, `Requires=`
on a not-found unit failing with `dependency`, signalling through
pidfds and `cgroup.kill` only. Exercise through a Go API first, without
D-Bus. Existing boot tests must pass unchanged; new tests for
start/stop/restart ordering, `Requires=` stop propagation, missing
`Requires=` vs `Wants=`, the transaction regression cases in
[Transactions and ordering](#transactions-and-ordering), and a PID-reuse
test for `KillUnit` (T10).

**Phase 4: control socket and object model (`internal/sdbus`,
`internal/systemctl`, godbus patch).**
Server handshake, peer-cred checks, object model. Tests: unit tests of
the handshake against recorded sd-bus bytes, and an integration test
job that installs real `systemctl` binaries from the Phase 0 distros
into a test image and runs each supported verb as a non-root user
against container-init. Include negative tests: other uid connects
(T11), `KillUnit` on an outside PID (T10), `Exit` refused.

**Phase 5: enable/disable, shim delegation, docs.**
`EnableUnitFiles`/`DisableUnitFiles` through the helper (T19), shim
delegation, README section, `--validate` support for checking a user's
unit directory as that user.

## Open questions

1. **`no_new_privs` default.** Safer, but breaks user services that call
   `sudo`. Proposal: on by default with an image-level opt-out. Needs a
   decision.
2. **Which users can be opted in by flag.** Should
   `--user-managers=all` exist (every passwd entry with UID ≥ 1000 and
   an existing home)? Convenient for single-user workspaces, but
   `/etc/passwd` can hold users the image author never meant to run code
   for.
3. **Does an image need `systemctl` installed?** Yes, the binary must be
   in the image. On Debian/Ubuntu the `systemctl` package ships it
   without making systemd PID 1. This collides with images that install
   docker-systemctl-replacement at `/usr/bin/systemctl`; document it.
4. **Fix the system supervisor too?** Behaviours this design avoids in
   user scope are also present for system units: a missing `Requires=`
   target is silently skipped; `Environment=` overrides
   `EnvironmentFile=` (systemd has it the other way round); a service
   can fork before it is moved into its cgroup; `Requires=` is treated
   as an ordering edge, so `Requires=` plus `Before=` on the same unit
   is rejected as a cycle; and `cmd.Start` can block on image paths
   (`WorkingDirectory=`, `ExecStart=`) while the dispatcher lock is
   held. The fix for the last one is the trusted exec wrapper, not
   dropping the lock (see
   [The user-side helper](#the-user-side-helper)). The first, second and fourth change behaviour for
   existing images, so each wants its own change and release note.
5. **Session bus without the shim.** If the image runs a session
   `dbus-daemon` but not the shim, should the user manager also claim
   `org.freedesktop.systemd1` on it? Only matters for clients that skip
   the private socket; defer until one shows up.
