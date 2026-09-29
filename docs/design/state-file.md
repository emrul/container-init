# Design: unit-state file and `container-init health`

Status: accepted
Date: 2026-09-29 (revised 2026-09-30 for the lifecycle-audit fixes)
Origin: R10 in workspaces-core-images `design/container-init-requests.md`

## Summary

A Docker `HEALTHCHECK` can probe what a container serves (a port, a
display), but not what only PID 1 knows: whether a oneshot finished,
or whether a service has stopped for good. container-init has no way
to report that today (no status command, no socket).

Two pieces:

- `--state-file <path>`: PID 1 writes every unit's state to a JSON
  file, on every change and as a heartbeat.
- `container-init health`: a subcommand that reads the file and exits
  0 or 1, for use as, or inside, a `HEALTHCHECK`.

The file only reports. Which units a container needs is the image's
policy, passed to `health` as flags; PID 1 never decides health.

## Goals

- A reader can tell, for every loaded unit: running, finished, waiting
  to restart, failed for good, not started yet, or never starting.
- A reader can tell a hung supervisor from a quiet one, within the
  limits in [Heartbeat](#heartbeat).
- An image gets a working health check with one `HEALTHCHECK` line,
  without parsing JSON itself.
- No new listener in PID 1.

## Non-goals

- An HTTP or other network endpoint. Docker's `HEALTHCHECK` runs a
  command inside the container, and Docker already exposes the result
  outside it (`docker inspect` `.State.Health`, the API, events). A
  listener in PID 1 would need bind, port and auth rules, would expose
  the unit inventory, and cuts against deny-by-default networking. If a
  live query is ever needed, it is a unix socket or `container-init
  status` reading the same file, never TCP.
- `result: timeout`. `TimeoutStartSec=` is parsed but not enforced,
  and a unit killed after its `TimeoutStopSec=` during shutdown is
  gone with the container.
- `Type=forking` beyond the parent's exit. The supervisor watches only
  its direct child, so a daemonised process reads as `inactive`.
- Control of any kind (start, stop, restart).

## The state file

`--state-file <path>` (empty, the default, disables it). PID 1 writes
the file at startup, before the first unit starts; on every unit state
change; and on the heartbeat, at least every 10 s, updating `written`.
Writes may be coalesced.

The shape borrows systemd's words, limited to what the supervisor
produces today. Example, with the window manager crashing into its
start limit and its `OnFailure=` target still running:

```json
{
  "version": 1,
  "written": "2026-09-29T17:03:12Z",
  "pid1": {"version": "v1.3.2", "started": "2026-09-29T17:00:00Z", "stopping": false},
  "units": {
    "kasm-setup.service":     {"type": "oneshot", "active": "active",     "sub": "exited",    "result": "success",         "runs": 1, "restarts": 0, "since": "2026-09-29T17:00:01Z"},
    "kasmvnc.service":        {"type": "simple",  "active": "active",     "sub": "running",   "result": "success",         "runs": 1, "restarts": 0, "since": "2026-09-29T17:00:02Z"},
    "window-manager.service": {"type": "simple",  "active": "failed",     "sub": "failed",    "result": "start-limit-hit", "runs": 5, "restarts": 4, "since": "2026-09-29T17:03:10Z"},
    "recorder-drain.service": {"type": "oneshot", "active": "activating", "sub": "start",     "result": "success",         "runs": 1, "restarts": 0, "since": "2026-09-29T17:03:10Z", "activation": "on-failure"},
    "upload.socket":          {"type": "socket",  "active": "active",     "sub": "listening", "result": "success",         "runs": 1, "restarts": 0, "since": "2026-09-29T17:00:01Z", "service": "upload.service"},
    "upload.service":         {"type": "simple",  "active": "inactive",   "sub": "dead",      "result": "success",         "runs": 0, "restarts": 0, "since": "2026-09-29T17:00:00Z", "activation": "socket", "sockets": ["upload.socket"]},
    "session-syslog.service": {"type": "simple",  "active": "inactive",   "sub": "dead",      "result": "success",         "runs": 0, "restarts": 0, "since": "2026-09-29T17:00:00Z", "never": "condition"}
  }
}
```

Every loaded unit is listed. Timestamps are RFC 3339, UTC, whole
seconds.

### Fields

- `version`: the file format, 1. A reader refuses a version it does
  not know.
- `written`: when the heartbeat last passed its checks (see
  [Heartbeat](#heartbeat)).
- `pid1.version`: the string `-version` prints, `v` included.
- `pid1.started`: when PID 1 started.
- `pid1.stopping`: true once PID 1 begins shutdown.
- `type`: `simple`, `oneshot` or `forking` (what the parser accepts),
  or `socket` for socket units.
- `active`: `inactive`, `activating`, `active`, `deactivating` or
  `failed`. `failed` means done and not restarted: the last run failed
  and `Restart=` is off or its start limit was hit. A service waiting
  out `RestartSec=` is `activating`/`auto-restart`.
- `sub`: services `dead`, `start`, `running`, `exited`,
  `auto-restart`, `stop`, `failed`; sockets `listening` or `failed`.
  There is no socket `running`: in native mode PID 1 cannot see
  connections, and the attached service's own entry already says
  whether it is up.
- `result`: the last run's `success`, `exit-code`, `signal` or
  `start-limit-hit`; or `resources` for a start that fails before the
  process runs (missing binary, unreadable `EnvironmentFile=`,
  unresolvable `User=`) and for a socket that cannot bind. A failed
  socket also has `trigger-limit-hit` (its own trigger limit),
  `service-start-limit-hit` (its service hit its start limit) or
  `dependency` (a unit its service requires failed), systemd's words
  where it has them. `success` while `runs` is 0, as systemd reports
  it; it says nothing then.
- `runs`: how many times the unit has been started since boot, by any
  path (its own loop, its socket, an `OnFailure=` trigger); for a
  socket, how many times it was bound. Counted when the start limit
  admits the start, so a start that fails with `resources` counts,
  and one the start limit refuses does not. An `OnFailure=` trigger
  that finds its target already running is dropped (logged "already
  active", as systemd ignores a start of an active unit) and does not
  count either. `runs` 0
  is the only reliable "has not started"; `since` is not, because
  whole-second timestamps can coincide and it marks state changes, not
  runs.
- `restarts`: automatic restarts since boot. Five starts under
  `StartLimitBurst=5` are four restarts.
- `since`: when `active`/`sub` last changed; PID 1's start for a unit
  that has not changed.
- `never`, on a unit that will not start this boot: `condition` (a
  failed `Condition*=`), `dependency` (a `Requires=` unit failed, or,
  for a socket-activated service, a socket of its failed to bind), or
  `missing-requirement` (a `Requires=` unit is not loaded). A failed
  unit stays failed for the rest of the boot, so this is final. An
  `OnFailure=` target gets it when a trigger finds one of its own
  requirements failed or missing: every later trigger would too.
- `activation`, on a unit something else starts: `socket` (its socket
  starts it on the first connection) or `on-failure` (it runs only
  when a unit naming it in `OnFailure=` fails).
- `service`, on every socket: the service it activates, as resolved at
  load time (`Service=`, or the socket's own name with `.service` in
  place of `.socket`). Present even when no unit of that name is
  loaded.
- `sockets`, on a service with `activation: socket`: every loaded
  socket whose `service` names it, sorted. Derived from the sockets'
  `service`, never from matching names.

### Oneshot lifecycle

A oneshot is `activating`/`start` for the whole of its run, never
`active` while it runs. When it exits, whether `Restart=` will start
it again decides the state first:

| Outcome | `active`/`sub` | `result` |
|---|---|---|
| Success, restarting (`Restart=always`) | `activating`/`auto-restart` | `success` |
| Success, not restarting, `RemainAfterExit=yes` | `active`/`exited` | `success` |
| Success, not restarting, no `RemainAfterExit=` | `inactive`/`dead` | `success` |
| Failure, restarting | `activating`/`auto-restart` | the failure |
| Failure, not restarting | `failed`/`failed` | the failure |

Only the two "not restarting" success rows are complete. A oneshot
under `Restart=always` is never complete: it is `activating` while it
runs and during every `RestartSec=` wait, so it never passes `health`.
`RemainAfterExit=yes` does not stop the restart, because
`RemainAfterExit=` has no runtime effect in container-init today; here
it only changes what is reported.

`inactive`/`dead`/`success` means "finished" when `runs` is at least
1, and "not started" (or, with `activation`, "not triggered") when
`runs` is 0.

Services follow the same rule: a clean exit under `Restart=always` is
`activating`/`auto-restart` with `result: success`, not
`inactive`/`dead`.

### Where this differs from systemd

- **Signal deaths are failures.** container-init counts any death by
  signal as a failure, SIGTERM included, and `Restart=on-failure`
  restarts it; the file says `result: signal`. systemd treats SIGHUP,
  SIGINT, SIGTERM and SIGPIPE as clean exits (`success`, no restart
  under `on-failure`). A program that catches SIGTERM and exits 0 is
  `success` in both.
- **No implicit start limit.** A unit that sets neither
  `StartLimitBurst=` nor `StartLimitIntervalSec=` has no limit, so
  under `Restart=on-failure` or `always` it never reaches `failed`; it
  cycles through `activating`/`auto-restart`.
- **A socket whose service's requirement failed closes.** systemd
  keeps it listening and fails each start job; container-init fails
  the socket (`result: dependency`) and closes its listener, since the
  service can never start this boot.
- **A native socket must be its service's only socket.** A native
  activation passes the service only its own listener, so container-
  init refuses, at startup and in `--validate`, a unit set where a
  native socket shares its service with any other socket; systemd
  instead passes the service all of its sockets. Several proxy sockets
  may share a service. Every unit has at most one process (a start
  while it runs waits, or for an `OnFailure=` trigger is dropped), so
  one entry per unit is the whole truth; `sockets` then lists every
  proxy socket, and `health` requires all of them.
- **`Restart=always` on a oneshot is accepted.** systemd refuses it
  at load time (it allows only `no` and `on-failure` for
  `Type=oneshot`; worth confirming against the systemd version you
  compare with); container-init runs it in a loop, as in the table
  above.
- **`runs`, `never` and `activation`** are container-init's own
  fields. systemd spreads the same facts over job results, unit
  dependencies and `NRestarts`.

A socket re-arms as in systemd: once its service stops and `Restart=`
will not start it again, the socket returns to `listening` and the
next connection starts the service anew. It fails, closing its
listener, when the service hits its start limit, when the service's
requirement failed, or when its own trigger limit is spent
(`TriggerLimitBurst=` / `TriggerLimitIntervalSec=`, default 20 per
2 s).

## Heartbeat

`written` shows that the supervisor is still doing its job, not only
that the process exists, but only as far as the checks below reach.

`written` starts as `pid1.started`. The first write, before the first
unit starts, sets it only if checks 1 and 2 pass (check 3 has nothing
to look at yet); otherwise it stays at `pid1.started` and ages out
like any missed heartbeat. After that, only a passing tick advances
it: a write made for a state change carries the last `written`
forward unchanged.

On each tick (every 10 s), the writer advances `written` only if all
of these pass within 2 s:

1. **State lock.** It takes the supervisor lock to snapshot unit
   state. A deadlocked supervisor fails here.
2. **Reaper.** It sends a ping through the dispatcher's loop and waits
   for the reply. The loop answers between drains, and a drain holds
   the dispatcher lock, so a wedged reaper or a stuck `Spawn` fails
   here.
3. **Exits noticed.** For every unit recorded `running` (or oneshot
   `start`), its pid must exist and not be a zombie (the state field
   of `/proc/<pid>/stat`). A zombie, or a pid that has vanished while
   the state still says running, means an exit was not reaped or not
   handled. One such tick can be the normal gap between exit and
   handling; the check fails when the same pid is seen that way on two
   ticks in a row.

The timeouts must not leave anything behind. The writer never starts
a goroutine per check that could block on a stuck lock or loop and
pile up tick after tick:

- The state lock is taken with `TryLock`, retried every 50 ms until
  the 2 s deadline, from the writer goroutine itself. Nothing waits on
  the lock after the deadline.
- The dispatcher ping is one buffered channel of capacity 1. The
  writer sends without blocking, carrying a sequence number, and
  waits up to 2 s for that number back on a reply channel that the
  loop also sends to without blocking. If the previous ping is still
  unanswered, the send fails and the tick fails; no second ping is
  queued. A late reply with an old number is discarded.

So however long the supervisor stays stuck, the heartbeat holds at
most one pending ping and no extra goroutines.

When a check fails, the writer logs which one (once per failing
stretch), still writes state changes if it can, and leaves `written`
alone. After three missed ticks, `health` reports the container
unhealthy.

What a fresh `written` does not prove: that a unit goroutine waiting
on something slow (a hung `WorkingDirectory=` mount, a dependency that
has not settled) will get anywhere, and that a running service is
doing useful work. The first is visible as `activating`; the second is
what the image's own probes are for.

## File safety

- Write an exclusive temporary file in the target's directory
  (`os.CreateTemp`, `O_EXCL`), then rename it over the target. Never
  open a fixed temporary name, never chmod or chown by path. The file
  is 0644.
- If the target's directory does not exist, PID 1 creates it (and
  any missing parents) 0755, owned by PID 1's uid. It never changes
  the mode or owner of a directory that already exists. `/run` is
  usually a tmpfs that starts empty, so a directory made at image
  build time is not there at runtime; this is why PID 1 creates it.
- When PID 1 is root and the directory is writable by another uid,
  PID 1 logs a warning once: that user can replace the file and forge
  the report. Writing stays safe (the exclusive temp file and rename
  never follow a planted name), only the contents cannot be trusted.
- `os.CreateTemp` creates the file 0600; the writer sets 0644 with
  `Chmod` on the open file, before the rename, so no mode or owner is
  ever set by path.
- A write that fails (disk full, read-only filesystem) is logged once
  and retried on the next change or heartbeat. PID 1 keeps
  supervising and never exits over it.
- Writing never holds up supervision, as for the trace. A state change
  only marks the state dirty and signals the writer through a channel
  of capacity 1 with a non-blocking send; the writer, alone, takes the
  snapshot and does the I/O. A writer stuck in I/O (a hung disk) costs
  a stale file, which the heartbeat's `written` then shows, never a
  stuck unit.
- At shutdown, `pid1.stopping` is written as soon as shutdown begins,
  and the final write, with every unit stopped, is attempted within
  reverse shutdown's own deadline (the one given to the tracer): PID
  1 waits for it no longer than that, and adds no time of its own.

Pick a path in a directory only PID 1's uid can write:

- Non-root PID 1: a directory the container user owns, for example
  `/run/kasm/container-init.json`. That user can replace the file,
  but they are PID 1's user already; it only hides their own
  container's health.
- Root PID 1: `/run/container-init/state.json`, which PID 1 creates
  root-owned.

## `container-init health`

```
container-init health --state-file <path> [--require <unit>,...] [--max-age 30s]
```

Reads the state file, prints one line saying why, and exits 0
(healthy) or 1 (unhealthy). It never exits 2, which Docker reserves.
Any error (missing file, bad JSON, unknown `version`) is unhealthy.

Unhealthy when any of:

- `written` is older than `--max-age` (default 30 s, three missed
  heartbeats).
- `pid1.stopping` is true.
- With `--require`: a named unit is missing from the file, or does
  not pass the rule for its kind below.
- Without `--require`: any unit is `failed`.

`--require` narrows the check to the named units: an optional unit
that fails does not make the container unhealthy.

A required unit passes when:

| Unit | Passes | Fails |
|---|---|---|
| service | `active` | anything else |
| oneshot | `active`/`exited`; or `inactive`/`dead`/`success` with `runs` ≥ 1 (finished) | `activating` (still running or restarting), `runs` 0, `failed`, `never` |
| service with `activation: socket` | `active`; or `inactive` while every socket in its `sockets` is `active` (no connection yet, or stopped and waiting for the next one) | `activating`, `failed` (its last run failed), `never`, any of its sockets not `active` |
| socket | `active`, and the unit its `service` names is in the file and passes the row above | the socket is not `active`; its service fails; its service is not loaded |
| unit with `activation: on-failure` | `inactive` with `runs` 0 (not triggered), or finished as a oneshot | `activating`, `failed` |

So a required setup oneshot is unhealthy for as long as it runs, and
healthy only once it has succeeded; it no longer needs
`RemainAfterExit=yes` to be checkable, but it must not restart on
success. Requiring either a socket or its service catches a service
that has failed behind a socket that is still listening, and a socket
that has failed.
Both directions use the recorded relationship (`service` on the
socket, `sockets` on the service), never the unit names.

A unit in `never` or waiting in `activating` fails the check. During
boot that is what `HEALTHCHECK --start-period` is for; a brief
`auto-restart` is absorbed by `--retries`.

Example, with image-specific probes in the image's own script:

```dockerfile
HEALTHCHECK --interval=30s --start-period=60s --retries=3 \
  CMD container-init health --state-file /run/kasm/container-init.json \
      --require kasm-setup.service,kasmvnc.service,window-manager.service,upload.socket \
   && kasm-health
```

`upload.socket` is there to show the socket rule. Whether uploads are
essential to a session is the image's choice; an image that can live
without them leaves it out.

`health` is dispatched on `os.Args[1]` before `flag.Parse`, after
`execwrap.Main()`, which only acts on its own marker. It reads a file
and compares names, counters and timestamps; it touches no PID 1 state
and needs no privileges beyond reading the file.

## Implementation notes

- The per-unit state has to be its own, updated in
  `spawnAndWait`/`finish`, the restart loops, `startLimitHit`,
  `fireOnFailure`, the dependency-failure paths and `shutdown`.
  `s.failed` records only units that never became ready (a
  `Type=simple` that started and later died stays "ready"), and
  `serviceState.restarts` is never incremented today.
- `runs` is incremented where a start is attempted, next to
  `allowStart`, so every start path counts it once.
- One writer goroutine owns the file: state changes signal it without
  blocking (see [File safety](#file-safety)), it coalesces them and
  runs the heartbeat. The dispatcher gains a ping
  channel in its loop's `select` for check 2, answered between
  drains.
- `service` comes from the parsed socket unit (`u.Service`, already
  defaulted in `unit/validate.go`); `sockets` is built once at load by
  inverting it over the loaded sockets.
- `encoding/json` sorts map keys, so the unit order is stable.

## Tests

- With the flag set, the file exists before the first unit starts,
  and `written` advances every 10 s while nothing changes.
- A oneshot is `activating`/`start` while it runs; `health --require`
  on it exits 1 during the run and 0 after it succeeds, with and
  without `RemainAfterExit=yes`.
- An `OnFailure=` target shows `activation: "on-failure"`,
  `inactive`/`dead`, `runs` 0 until its unit fails; after it completes
  it shows `inactive`/`dead`/`success`, `runs` 1.
- SIGKILL a `Restart=on-failure` service's process: `restarts` and
  `runs` go up by one and the unit returns to `active`/`running`. (Do
  not try to catch `auto-restart`; a short `RestartSec=` makes it too
  brief.)
- Crash a unit with `StartLimitBurst=5` five times within its
  interval: `failed`/`start-limit-hit`, `runs` 5, `restarts` 4.
- A oneshot with `Restart=always` that exits 0 shows
  `activating`/`auto-restart`, `result: success`; `health --require`
  on it exits 1.
- A socket with an explicit `Service=` naming a service of a different
  stem: the socket's `service` and the service's `sockets` name each
  other, and `health --require` on either follows the other. A socket
  whose `Service=` is not loaded: `health --require` on it exits 1.
  Two proxy sockets for one service: `sockets` lists both, and
  requiring the service fails while either socket is not `active`.
- An `OnFailure=` target triggered while it is still running: the
  second trigger leaves `runs` unchanged. One whose own `Requires=`
  unit has failed: `never: "dependency"`, `runs` 0.
- A socket that cannot bind: the socket is `failed`/`resources`, and
  its service shows `never: "dependency"`.
- A socket-activated service that exits cleanly: the socket stays
  `active`/`listening` and `health --require` on either exits 0. One
  that fails under `Restart=no`: the socket still listens, and
  `health --require` on either exits 1 until a later run succeeds.
  One driven past its start limit: the socket is
  `failed`/`service-start-limit-hit`, and both exit 1. Before the
  first connection both exit 0.
- A unit whose `Requires=` failed shows `never: "dependency"`; one
  whose `Requires=` is not loaded shows `never: "missing-requirement"`;
  one with a failed condition shows `never: "condition"`.
- Heartbeat: with the dispatcher loop blocked (a test hook), `written`
  stops advancing and `health` exits 1 once it is older than
  `--max-age`; the same when a unit's process is reaped but its state
  is held at `running` (a test hook), after two ticks. While blocked
  for many ticks, the goroutine count stays flat and at most one ping
  is outstanding.
- With the dispatcher ping blocked from startup (a test hook), the
  first file's `written` equals `pid1.started` and never advances; a
  state-change write while checks are failing leaves `written`
  unchanged.
- A missing state-file directory is created 0755; an existing one is
  left alone.
- With the flag unset, nothing is written.
- With the writer blocked in I/O (a test hook), units still start and
  stop, and shutdown returns within its deadline.
- `health`: exits 1 on a stale `written`, on `stopping`, on a required
  unit that is missing, and on a missing or malformed file.
