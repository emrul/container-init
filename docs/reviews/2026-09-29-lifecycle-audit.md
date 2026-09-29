# Lifecycle, process ownership, configuration, and reporting audit

Reviewed commit: `7aa3153` (2026-09-29).

Production code is unchanged. This review adds 12 regression tests across three
files. Eleven tests fail on the reviewed commit; one disproves a suspected output
descriptor leak. An existing Linux test also exposes a data race. There are 11
findings below (the two environment parsing tests support one finding).

These are failures of the current implementation, not a claim that every issue
was introduced in the last release. The previously reported socket/shutdown
corrections remain covered by their existing regression tests.

## Findings

### 1. P1 — SocketMode is accepted but the socket is made world-writable

`unit/parse.go` accepts `SocketMode=0600` without warnings, including under
`Strict: true`. `internal/socketact/listen.go:56` always chmods the Unix socket to
0666. This silently removes the access restriction the image specified. The
comment describes socket permissions as future work, but validation presents
the directive as supported today.

Proof: `TestAuditSocketModeIsEnforced` in
`internal/supervisor/audit_regression_test.go` observes 0666 for a parsed 0600
socket, as root and as uid 65534.

Fix direction: apply the requested permissions when binding, or reject/warn on
unsupported permission directives so strict validation cannot approve them.

### 2. P1 — Failed cgroup setup does not actually fall back to process-group killing

`spawnAndWait` logs a PGID fallback when `cgroup.Mkdir` fails. However,
`finish` and `stopUnit` choose their kill path using the manager-wide
`Available()` flag, which remains true. They then ignore errors from killing the
missing cgroup. A service can survive a shutdown reported as force-killed.

Proof: `TestAuditCgroupPlacementFailureStillKillsProcess` detects a writable
cgroup manager, overlays its base with a read-only mount inside a disposable
privileged container, starts a service that ignores TERM, and requests shutdown.
After the deadline and a "force-killed" log, the PID is still alive. The test
kills it explicitly and removes the injected mount. It requires the explicit
`CONTAINER_INIT_AUDIT_PRIVILEGED=1` environment variable.

Fix direction: track placement success per process and use a real fallback when
placement or cgroup killing fails. Do not report successful teardown solely
because a kill was attempted. Container PID 1 exiting would eventually cause
kernel cleanup, but the supervisor's ordering/teardown guarantee is already broken.

### 3. P1 — The global shutdown timeout does not bound waiting for spawnGate

`shutdown` takes `spawnGate.Lock()` before creating the deadline timer or running
any stop jobs (`supervisor.go:1128`). A spawn holds the read lock across trace
writes, process creation, placement, and bookkeeping (`supervisor.go:625`). A
blocked trace destination can therefore prevent even existing services from
being signalled, indefinitely beyond `--stop-timeout`.

Proof: `TestAuditShutdownDeadlineIncludesSpawnGate` keeps one service alive and
fills a trace FIFO with another spawn's event. A 100 ms shutdown is still blocked
after 350 ms. Draining the FIFO lets shutdown proceed. The test releases the
fault and waits for all processes/goroutines before finishing. This reproduces
as root and non-root; tracing must be enabled for this particular trigger.

Fix direction: keep unrelated I/O out of the spawn-registration critical section
and ensure the deadline can initiate cleanup while registration is stalled.

### 4. P2 — Concurrent invocations of one OnFailure target lose process ownership

`fireOnFailure` can spawn the same unit concurrently, while
`services[u.Name] = state` stores only the newest process. With cgroups disabled,
shutdown visits only that record and misses the earlier process. Two services
sharing a long-running failure handler can produce this situation.

Proof: `TestAuditConcurrentOnFailureShutdownOwnsEveryProcess` invokes the same
handler twice, then shuts down. The first PID remains alive after shutdown; the
test explicitly kills and reaps it. Reproduced as root and non-root with the
fallback manager.

Fix direction: serialize/coalesce activation by unit identity, or explicitly
track every invocation through teardown. A single map entry cannot own two PIDs.

### 5. P2 — A failed socket bind releases required dependents as successful

`bindSocket` returns without a result on bind failure. `Run` nevertheless calls
`signalReady` on the socket and its attached service (`supervisor.go:292`). A
consumer with both `Requires=the.socket` and `After=the.socket` starts even though
the socket could not bind.

Proof: `TestAuditFailedSocketBlocksRequiredService` occupies a TCP address before
boot. The socket logs EADDRINUSE, but its requiring consumer still writes its
execution marker. Reproduced as root and non-root.

Fix direction: propagate bind failure into dependency settlement and signal
readiness only on successful binding. This is separate from the recently added
service-failure-to-socket-failure propagation.

### 6. P2 — OnFailure activation bypasses the handler's own Requires constraints

`fireOnFailure` checks conditions and limits, then calls `spawnAndWait` directly.
It does not use `waitDeps` or check `missingReq`. Thus the documented rule that a
missing required unit prevents a service from starting does not apply to this
activation path.

Proof: `TestAuditOnFailureHonoursMissingRequirement` triggers a handler requiring
`not-installed.service`. Its execution marker is created. Reproduced as root and
non-root.

Fix direction: route failure handlers through the same dependency admission
rules as ordinary and socket-activated services, while preserving shutdown guards.

### 7. P2 — Native socket re-arming races listener closure during shutdown

The Linux race detector reports a read in `driveNative` at
`supervisor.go:790` (`bound.File.Fd()`) concurrent with `Bound.Close` at
`internal/socketact/listen.go:71` (`b.File.Close()`), called by an ordered stop job.
`sync.Once` serializes close calls, but it does not synchronize them with the
activation driver's descriptor access.

Proof: the existing `TestSocketRearmsAfterServiceStops/native/exit-0` fails under
Linux/amd64 Go 1.27.1 `-race`; the same suite passes without the race detector.
Relevant detector stack:

```text
Write: internal/poll.(*FD).destroy -> os.(*File).Close
       -> socketact.(*Bound).Close.func1 (listen.go:71)
       -> supervisor.(*Supervisor).shutdown.func2
Read:  os.(*File).fd -> os.(*File).Fd
       -> supervisor.(*Supervisor).driveNative (supervisor.go:790)
```

Fix direction: synchronize the native driver's exit with listener closure and
descriptor lifetime. Copying an fd number alone does not protect it from reuse.

### 8. P2 — Group without User is silently ignored

The entire credential path is guarded by `if u.User != ""`
(`supervisor.go:557`). A service specifying only `Group=` neither changes group
under root nor fails admission under a non-root supervisor requesting a different
group. The README advertises `User= / Group=` privilege handling without this
restriction.

Proof: `TestAuditGroupWithoutUserIsEnforced` requests gid 1 as root and observes
gid 0. As uid/gid 65534, a request for gid 65535 succeeds instead of being refused.

Fix direction: resolve Group independently with the supervisor's uid as the
default, or reject unsupported group-only configuration explicitly.

### 9. P2 — Identity environment precedence depends on duplicate entries

`spawnAndWait` promises that the resolved User identity wins for HOME, USER and
LOGNAME. It first appends directive/file environment entries to the inherited
environment, then `setEnv` replaces only the first matching key. A later
duplicate wins when the command environment is deduplicated.

Proof: `TestAuditUserIdentityEnvironmentWinsDuplicates` supplies inherited and
directive values plus a valid current-user identity. The child sees
`/directive|directive|directive`, rather than the resolved passwd values, as both
root and non-root.

Fix direction: enforce the chosen policy consistently by removing/replacing all
duplicates, or intentionally revise the precedence contract and tests. This
finding uses the implementation's stated policy, not an assumption that every
systemd environment rule must be copied.

### 10. P2 — Environment parsing corrupts accepted quoted values

`Environment="GREETING=hello world"` passes strict parsing but becomes two
entries with literal quotation marks because `splitWords` uses `strings.Fields`.
Separately, `parseEnvValue` trims the final assembled value, including whitespace
inside quotes, despite its comment promising to preserve quoted whitespace.

Proofs in `unit/audit_regression_test.go`:

- `TestAuditQuotedEnvironmentAssignment`: expected `GREETING=hello world`, got
  `"GREETING=hello` and `world"` as separate entries.
- `TestAuditQuotedEnvironmentFilePreservesTrailingSpace`: a single-quoted value
  ending in a space loses that space.

Fix direction: tokenize Environment assignments with quote awareness and strip
only unquoted whitespace in environment files. Credentials and arguments that
contain spaces must not change silently.

### 11. P2 — D-Bus object-path encoding aliases distinct unit states

`escapeUnitName` escapes `-` as `_2d` but leaves `_` unchanged. Consequently
`app-a.scope` and `app_2da.scope` share one `unitSet` key. Starting the second
returns the first unit's Id, and stopping the second deletes the first's state.
This became observable when the shim began storing active-unit properties.

Proof: `TestAuditUnitNameEscapingIsInjective` in
`internal/systemd1shim/audit_regression_test.go` exercises the manager and property
stub directly and fails both identity and stop-isolation assertions.

Fix direction: use an injective, systemd-compatible object-path encoding,
including escaping literal underscores. The shim's intentionally synthetic
active state and 1024-entry eviction policy were not treated as bugs.

## Verification and limits

| Check | Result |
|---|---|
| Existing supervisor suite, static Linux/amd64, root, privileged cgroup v2 | Pass, including direct-cgroup spawn and seccomp fallback tests |
| Existing supervisor tests, static Linux/amd64, uid/gid 65534 | No existing test failures; privileged-only tests skip |
| New supervisor audit tests, root | 8 fail as described, descriptor ownership control passes |
| New supervisor audit tests, uid/gid 65534 | 7 fail as described, privileged mount test skips, descriptor control passes |
| New parser and shim tests on macOS | 3 fail as described |
| Existing packages, macOS `go test -race ./... -skip TestAudit` | Pass |
| Existing supervisor suite, Linux/amd64 Go 1.27.1, `-race` | Fails with the native listener-close race; cgroup tests skip in this non-privileged run |
| `go vet ./...` on macOS | Pass |

The descriptor hypothesis was disproved on this toolchain: 32 completed spawns
with GC disabled did not accumulate output descriptors. Its passing test is kept
as a control. Existing tests also exercised wrapper exec failures, orphan churn,
supplementary-group resolution, environment-file precedence, socket restarts,
start/trigger limits, dependency ordering and per-unit/global stop timeouts.

This was a targeted review of the four agreed areas, not an exhaustive proof of
all interleavings or deployment environments. Linux execution was amd64; no
Linux/arm64 runtime matrix was performed. Full D-Bus client integration was not
rerun; the newly confirmed reporting issue is covered by the in-process manager
and property tests. Known limitations such as ExecStop not running and
TimeoutStartSec not being enforced were not reclassified as new bugs.

## Reproduction

The new tests assert desired behavior and intentionally fail until production
fixes land. They are not skipped or inverted to keep CI green. To distinguish
existing coverage from the new findings:

```sh
rtk go test ./... -skip TestAudit
rtk proxy go test ./unit ./internal/systemd1shim -run TestAudit -count=1
```

Build the supervisor tests for Linux. The binary can be copied to a Docker
container, so the Docker daemon need not share the host filesystem:

```sh
rtk proxy env GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go test -c -o /tmp/container-init-audit.test ./internal/supervisor
rtk proxy docker create --name container-init-audit \
  --privileged --cgroupns=private -e CONTAINER_INIT_AUDIT_PRIVILEGED=1 \
  ubuntu:24.04 /tmp/supervisor.test -test.run TestAudit -test.v -test.timeout 45s
rtk proxy docker cp /tmp/container-init-audit.test container-init-audit:/tmp/supervisor.test
rtk proxy docker start -a container-init-audit
rtk proxy docker rm container-init-audit
```

For the non-root case, use `--user 65534:65534` instead of the privileged flags and
omit the audit environment variable. The mount-injection test deliberately skips.
For the existing root suite, use `-test.skip TestAudit` instead of `-test.run`.

On a Linux host with Go 1.27.1 and a C compiler, the data race is exercised by:

```sh
rtk proxy go test -race ./internal/supervisor -skip TestAudit -count=1
```

The race is scheduling-dependent; repeat the native re-arming test if needed.
Full captured container output from this run is also available locally under
`/tmp/container-init-audit-results/`.

## Resolution

All 11 findings are fixed on `socket-rearm-and-shutdown`, each commit
carrying the audit tests for its findings so no commit has a knowingly
failing test.

| Commit | Findings | Notes |
|---|---|---|
| `92054ad` | 2, 3, 7 | Per-process `inCgroup` picks the kill path; trace output moved out of the spawn gate; the native driver polls through `Bound.WaitReadable`, locked against `Close`, with `poll(2)` |
| `8231eef` | 4, 5, 6 | Per-unit run lock (an active `OnFailure=` target is not started again, as in systemd); `OnFailure=` goes through `waitDeps`; a failed bind fails the socket and its service and fires the socket's `OnFailure=` |
| `8a33cf9` | 1, 8, 9 | `SocketMode=` / `SocketUser=` / `SocketGroup=` applied; `Group=` alone sets the gid; `setEnv` drops duplicates |
| `9509c2e` | 10, 11 | Quote-aware `Environment=`; quoted trailing space kept in env files; `escapeUnitName` is systemd's `bus_label_escape` |
| (group 5) | -- | `go test -race ./...` in CI; the privileged job opts in to, and requires, the cgroup-failure test |

Changes to the audit's tests:

- `TestAuditShutdownDeadlineIncludesSpawnGate` waited for the gate to be
  held during the blocked trace write, which the fix makes impossible.
  It now checks the guarantee instead -- a running service is stopped
  within the deadline while a spawn's trace write is blocked -- and
  releases the FIFO before failing rather than hanging in cleanup.
- `TestOnFailureCycleWithSpentLimits` (existing) counts an invocation
  coalesced as already active as its one refusal.

Found while fixing, not in the findings above:

- `select(2)` in the native driver indexed past its fd set for a
  descriptor >= 1024, panicking PID 1; replaced with `poll(2)`.
- A cgroup move (`Place`) that failed after a successful mkdir left the
  process outside its cgroup, with the same effect as finding 2.
- `fireOnFailure` also started a second copy of a target already
  running as an ordinary service; the run lock covers it.
- `SocketUser=` / `SocketGroup=` were ignored alongside `SocketMode=`.
- The cgroup spawn tests read `spawnIntoCgroupOK` without the `Once`,
  a test-only data race.

### Follow-up review

A review of the fixes above found three more issues:

- **P1: `SocketMode=0000` became 0666.** The unit used 0 for "unset",
  so an explicit `0000` got the default. The parser now records
  `SocketModeSet`, carried into `socketact.Perm.ModeSet`: omitted gives
  0666, anything written is applied exactly.
- **P2: a blocked trace destination still held up shutdown.** Tracing was
  synchronous, so `shutdown()`'s final `reverse_shutdown_done` record,
  and `main`'s exit after it, waited on the destination indefinitely.
  The tracer now encodes each record in the caller and queues it for a
  writer goroutine, never blocking: a full queue drops and counts
  records, reported in-stream as `trace_dropped`. `shutdown()` hands its
  deadline to the tracer (`SetDeadline`), and `Close` -- which `main`
  now calls before every `os.Exit` -- waits no longer than that
  deadline, or `trace.CloseWait` (1s) when none is set. A repeated or
  deferred `Close` is bounded the same way. No time is added to
  shutdown for flushing; the cost is that records queued when a forced
  shutdown hits its deadline may be lost even with a healthy writer.
- **The bind-then-chmod window.** A Unix socket is now bound in a
  `MkdirTemp` (0700) directory beside its path, given its owner and
  mode, and renamed into place -- atomic on the same filesystem, and
  replacing a stale socket from a previous run. On failure the staged
  node and directory are removed and the socket fails. A path that
  fits `sockaddr_un` only without the staging suffix is bound through
  `/proc/self/fd/<staging dir>`; without `/proc` (macOS, tests only)
  such a path fails. The umask is never changed, since it is
  process-wide.

Tests: `TestBindSocketMode` and `TestSocketModeFromUnitFile` (omitted,
`0000`, `0600`); `TestBindPublishesOnlyAfterPermissions` pauses between
staging and publishing under umask 000 and asserts the public path does
not exist and cannot be connected to, then checks owner, mode and
connectivity (as root with a foreign owner); `TestBindFailureCleansUp`,
`TestBindReplacesStaleSocket`, `TestBindLongPath`. For tracing,
`TestShutdownReturnsWithBlockedTrace` stalls the destination until
records are dropped and asserts that `shutdown()` and the tracer's
`Close` (twice) return within the deadline -- replacing
`TestAuditShutdownDeadlineIncludesSpawnGate`, whose fault injection
relied on a trace write blocking, which can no longer happen -- and
the trace package tests emitting into a stalled writer, `Close` bounded
by the deadline and by `CloseWait`, and exact accounting (records
written plus `Dropped()` equals records emitted; `trace_dropped`'s
`total` matches).

Two native sockets sharing one long-running service could hang: each
activation passes only its own listener, and the run lock makes the
second wait for the service to exit, so its clients can wait
indefinitely. The same holds for a native socket sharing its service
with a proxy one. Until activation passes every socket of the service
at start, as systemd does, such a unit set is rejected: `New` and
`--validate` (`supervisor.Check`) fail it with exit 64 and an error
naming the service and its sockets. Several proxy sockets may still
share a service, since a proxied connection does not wait for the run
lock.
