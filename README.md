# container-init

[![CI](https://github.com/emrul/container-init/actions/workflows/ci.yml/badge.svg)](https://github.com/emrul/container-init/actions/workflows/ci.yml)
[![Latest release](https://img.shields.io/github/v/release/emrul/container-init?style=flat)](https://github.com/emrul/container-init/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/emrul/container-init)](go.mod)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

A small PID 1 supervisor for container images. Reads a documented
subset of systemd unit files, supervises services, and provides
socket activation in two modes (native via `sd_listen_fds`, and proxy
for unmodified upstream binaries).

The binary is generic; image-specific behaviour lives in the unit
files an image author drops at `/etc/container-init/units/` and
`/etc/container-init.d/`.

## Motivation

This project was inspired by
[docker-systemctl-replacement](https://github.com/gdraheim/docker-systemctl-replacement),
a Python script that replaces `/usr/bin/systemctl` inside containers so that
standard systemd unit files work without a full systemd daemon. It solves the
right problem, but it carries a Python runtime dependency and starts services
sequentially, both of which add friction and latency.

container-init addresses those constraints:

- **No runtime dependency.** A single statically-linked binary (≤ 8 MiB, no
  CGO); no Python, no interpreter, no shared-library surprises.
- **Faster cold starts.** Services with no inter-dependencies start
  concurrently. Socket activation lets the binary bind public ports immediately
  and defer heavyweight services until the first real connection.

### PID 1 problems -- also solved here

The [PID 1 problems](https://github.com/gdraheim/docker-systemctl-replacement#problems-with-pid-1-in-docker) documented by `docker-systemctl-replacement` are solved the same way:

| Problem | How container-init addresses it |
|---|---|
| **Zombie accumulation** | `internal/pid1` owns a dedicated SIGCHLD-driven `wait4(-1)` loop. Every reparented grandchild -- from `dbus-launch`, `Type=forking` units, or any process that re-parents onto PID 1 -- is reaped silently and immediately. |
| **SIGTERM not reaching children** | `docker stop` sends SIGTERM to PID 1; the dispatcher forwards it to every supervised child before the stop timeout expires. |
| **Unordered / incomplete shutdown** | Reverse-dependency shutdown tears services down in reverse `After=` / `Requires=` order, giving each its `TimeoutStopSec` before SIGKILL. |
| **Service startup at boot** | The supervisor reads unit files and starts all services at launch, respecting `After=` / `Requires=` ordering -- no bespoke shell scripts needed. |

## Layout

```
cmd/
  container-init/      PID 1 supervisor binary
unit/                  unit-file parser + supported-directive validator + env expansion (public API)
internal/
  supervisor/          goroutine-per-service supervision
  socketact/           socket activation (native + proxy)
  cgroup/              per-unit cgroup-v2 placement + cgroup.kill teardown
  pid1/                SIGCHLD dispatcher, signal forwarding, reverse shutdown
  trace/               JSONL boot-trace emission
  userdb/              /etc/passwd + /etc/group resolution for User= / Group=
Makefile
```

`unit/` is the only exported package -- downstream images can import it
for property-level corpus tests against their unit files. Everything
else stays internal so the supervisor / socket-activation / cgroup
internals remain free to evolve.

## Building

```
make build      # produces bin/container-init.linux-{amd64,arm64}
make test
```

The binary is statically linked, CGO disabled, ≤ 8 MiB.

## Run-time layout (image side)

container-init reads from two directories, in priority order:

1. **`/etc/container-init/units/`** -- core unit files installed by the
   base image.
2. **`/etc/container-init.d/`** -- drop-ins shipped by derived images
   that layer on top of the base. Drop-ins go through the same parser
   and validator as core; they can reference core units via `After=`
   / `Requires=` / `OnFailure=`.

Both paths are configurable via the `--units` and `--drop-in` flags.

The `--strict-units` flag promotes any parser warning (unknown
directive / section, unsupported value form) into a fatal load error
-- useful in CI to catch typos before they ship.

The `--validate` flag loads + parses units, prints a summary, and
exits without supervising. Combined with `--strict-units`, this is a
build-time sanity check.

## systemd1 D-Bus compatibility shim

Some desktop apps detect "real systemd" by spawning `systemd-run` and
fail if the call doesn't succeed. The notable case is GNOME Ptyxis,
which wraps every shell it opens with
`systemd-run --user --scope …` -- if that returns non-zero, every
terminal tab shows up as "Terminal (Failed)". Older `gnome-terminal`
versions, GNOME apps that use Flatpak's `host-spawn`, and anything
that calls `systemd-run` from a shell script land in the same place.

container-init ships an optional shim that answers
`org.freedesktop.systemd1.Manager.StartTransientUnit` (plus the
supporting Start/Stop/Reload/Restart-Unit, GetUnit, ListUnits,
Subscribe, Reload, Reexecute, Introspect, and Properties surface for
unit objects) well enough to make `systemd-run` exit 0. It is a
**compatibility surface, not an implementation**: no cgroup is
created, no resource limits are enforced, properties like `Slice=`,
`MemoryMax=`, `CPUQuota=`, `PIDs=` are accepted and ignored.

### Two ways to run it

**A. Built into PID 1 (`--systemd1-shim` flag).** Suitable when the
target bus accepts ownership requests from root -- typically the
system bus or a permissively-configured custom bus. The session bus
owned by an unprivileged user (kasm-user, your-desktop-user, etc.)
will reject a root connection's `RequestName` via EXTERNAL auth's
uid-match check, so this path **does not** help for desktop session
buses.

```
ExecStart=/usr/local/bin/container-init --systemd1-shim=system
```

**B. Standalone binary (`/usr/local/bin/systemd1-shim`) supervised
as the bus owner.** This is the right shape for desktop sessions:
container-init runs the binary as the user who owns the session bus,
so EXTERNAL auth succeeds and `org.freedesktop.systemd1` is exposed
on the bus the user's apps already talk to. Ship a drop-in:

```ini
# /etc/container-init.d/systemd1-shim.service
[Unit]
Description=systemd1 D-Bus compatibility shim
After=session-setup.service
Requires=session-setup.service

[Service]
Type=simple
User=app-user
EnvironmentFile=/tmp/dbus.env  # exports DBUS_SESSION_BUS_ADDRESS
ExecStart=/usr/local/bin/systemd1-shim
Restart=on-failure
RestartSec=2s
```

With no `--address` flag the binary reads `$DBUS_SESSION_BUS_ADDRESS`
from its environment. Pass `--address=…` if you want to override.

### Bus-address forms

Both entry points accept the same address vocabulary:

| Form | Resolves to |
|---|---|
| `system` | the system bus (`$DBUS_SYSTEM_BUS_ADDRESS` or `/var/run/dbus/system_bus_socket`) |
| `user` | the calling uid's `$XDG_RUNTIME_DIR/bus` |
| `user:UID` | `/run/user/UID/bus` |
| `user:env:VAR` | `/run/user/$VAR/bus` -- reads VAR at start (e.g. `user:env:KASM_OS_UID`) |
| `dbus:env-file:PATH` | reads `DBUS_SESSION_BUS_ADDRESS` from a shell-syntax env file (handles `KEY='val';` and `export KEY=val`) |
| `unix:path=…` | any raw D-Bus address |

Multiple addresses can be comma-separated in the `--systemd1-shim`
flag; one goroutine per address dials with retry, reconnects on
disconnect, and stands down without disrupting a real systemd if
one is already on the bus.

### What it returns

- `StartTransientUnit` synthesizes a monotonic job id, emits
  `JobNew` + `JobRemoved{result:"done"}`, returns the job's object
  path. `systemd-run` blocks on `JobRemoved` before exec'ing its
  target, so the signal order matters.
- `Get(InvocationID)` on `/org/freedesktop/systemd1/unit/*` returns
  an empty `ay` (the "no recorded invocation" sentinel) so
  `systemd-run`'s post-StartTransientUnit query doesn't abort.
- Everything else returns zero values or empty arrays.

Callers that read property values for anything other than
"is this thing alive" will get back zero answers -- that's the
honest signal that this is a shim, not a unit-state store.

## Extension point -- `/etc/container-init.d/`

The first-class way for layered images to add their own services or
replace core ones.

### Override semantics

A drop-in named *identically* to a core unit (full filename match,
including the `.service` / `.socket` suffix) **replaces** the core
unit. container-init logs one line per replacement at startup:

```
container-init: drop-in override: web.service replaces /etc/container-init/units/web.service with /etc/container-init.d/web.service
```

The trace JSONL (when `CONTAINER_INIT_TRACE=1`) emits an
`unit_overridden` event so dashboards can spot overlays at a glance.

A drop-in with any *other* name is **additive** -- added to the unit
graph, parsed, supervised, and shut down alongside the core set.

### Naming conventions

- For an *intentional* override, name the drop-in identically to the
  core unit you want to replace. There is no namespacing; full
  filename match is the override key.
- Recommended convention for additive units:
  `<image-name>-<service>.service` --
  e.g. `chrome-launcher.service`, `vscode-server.service`.

### Validation

Drop-ins go through the same supported-directive validator as core
units. Unsupported directives produce a parse-time warning naming the
file + section + directive (default) or fail-fast (`--strict-units`).

### Restart, conditions, lifecycle

Drop-ins participate in the supervisor's full lifecycle:

- **After= / Requires= / Before=** -- start ordering. `Before=X` is
  folded into `X`'s `After=` when units are loaded, so it orders the
  boot sequence *and* blocks the target at run time, and it composes
  with an `After=` the target declares itself. Ordering a drop-in ahead
  of a core unit -- the usual reason to reach for it -- therefore needs
  no override of that core unit.
- **Failed dependencies** -- a unit fails when it cannot be started
  (bad `EnvironmentFile=`, unresolvable `User=`, `fork/exec` error) or
  is a oneshot that exits non-zero, and its `Restart=` policy gives up.
  Units that only order `After=` it then start anyway; units that
  `Requires=` it are not started and fail in turn, as in systemd. Each
  step logs a line (`failed to start`, `failed`, `not started: required
  unit X failed`). A dependency failure does not fire the dependent's
  own `OnFailure=` / `ExitContainerOnFailure=`.
- **Restart= / RestartSec= / StartLimitBurst= / StartLimitIntervalSec=** --
  per-unit restart policy and rate limiting. Every start counts, the
  first and each restart; a unit that would start more than
  `StartLimitBurst=` times within `StartLimitIntervalSec=` is not
  started again and fails (`OnFailure=` / `ExitContainerOnFailure=`
  fire, `Requires=` dependents fail if it never became ready). The
  limit directives belong in `[Unit]`, as in systemd, and are also
  accepted in `[Service]`. Setting one alone gets systemd's default
  for the other (5 starts / 10s), and `0` in either disables the
  limit. Unlike systemd, a unit that sets neither has no limit.
- **ConditionPathExists= / ConditionPathExistsGlob= / ConditionEnvironment= /
  ConditionUser=** -- unit is loaded but skipped at boot when conditions
  are unmet. `ConditionUser=` takes a uid or user name, optionally
  negated with `!`, and compares it with container-init's own uid --
  e.g. `ConditionUser=root` for a unit that needs a root PID 1,
  `ConditionUser=!root` for one that only makes sense without it. Names
  resolve at load time, before any unit runs; `root` needs no passwd
  entry. systemd's `@system` is not supported.
  A `.socket` whose service is skipped is skipped with it and does not
  listen, so a client cannot start a service its conditions ruled out.
- **OnFailure=** -- invoke a sibling oneshot when this unit fails for
  good: it failed and `Restart=` will not start it again, or it hit its
  start limit. A failure the unit restarts from does not fire it, as in
  systemd -- so under `Restart=always` / `Restart=on-failure` it fires
  only once a start limit runs out. Chains across the core/drop-in
  boundary identically.
- **ExitContainerOnFailure=true** -- fail-secure: take the whole
  container down via reverse shutdown on this unit's first failure,
  even one `Restart=` would have recovered from; `OnFailure=` fires on
  the way down. Useful for compliance gates an image author owns.
- **User= / Group= / WorkingDirectory=** -- privilege drop. Accepts
  the `${VAR:-default}` env-expansion form so per-image overrides
  flow through automatically.

  When container-init itself runs without root (the container is
  started with `--user`), it cannot switch identity at all. A `User=`
  unit that resolves to container-init's own uid and gid then runs
  as-is, keeping container-init's supplementary groups; one that wants
  any other identity fails to start with a log line naming both. One
  line at boot says switching is off. A numeric `User=` with no passwd
  entry gets gid 0 unless `Group=` is set, so under a non-root
  container-init such a unit needs an explicit `Group=` to match.

  `User=` / `Group=` names resolve against `/etc/passwd` and
  `/etc/group`, read directly (the binary is static and never uses
  NSS). Set `CONTAINER_INIT_PASSWD_FILE` / `CONTAINER_INIT_GROUP_FILE`
  to read other files instead -- for example the files an
  `nss_wrapper` setup generates to rename the user of a non-root
  container. They are read at each unit start, so a unit can generate
  them for the units after it.

## Worked examples

### 1. One-shot at boot

```ini
# /etc/container-init.d/myimage-init.service
[Unit]
Description=My image's per-session init
After=session-setup.service
Requires=session-setup.service

[Service]
Type=oneshot
RemainAfterExit=yes
User=${APP_USER:-app}
ExecStart=/usr/local/bin/myimage-init
TimeoutStartSec=60s
```

### 2. Long-running app

```ini
# /etc/container-init.d/myimage-app.service
[Unit]
Description=Background app
After=window-manager.service
Requires=window-manager.service
StartLimitBurst=5
StartLimitIntervalSec=60s

[Service]
Type=simple
User=${APP_USER:-app}
WorkingDirectory=${APP_HOME:-/home/app}
ExecStart=/usr/local/bin/myimage-app --listen 127.0.0.1:5000
Restart=on-failure
RestartSec=2s
```

### 3. Socket-activated helper, native mode

When the helper consumes `LISTEN_PID` / `LISTEN_FDS`, use native
activation. container-init binds the public port and hands the
listener fd over on first connect:

```ini
# /etc/container-init.d/myhelper.socket
[Unit]
Description=My helper -- public listener

[Socket]
ListenStream=5050
ActivationMode=native
Service=myhelper.service

[Install]
WantedBy=sockets.target
```

```ini
# /etc/container-init.d/myhelper.service
[Unit]
Requires=myhelper.socket

[Service]
Type=simple
User=${APP_USER:-app}
ExecStart=/usr/local/bin/myhelper
Restart=on-failure
RestartSec=500ms
```

The helper reads `LISTEN_PID` / `LISTEN_FDS` and uses
`os.NewFile(3, "listener")` to consume the inherited fd. Cold-start
lands on the first connection, not at boot.

### 4. Socket-activated helper, proxy mode

When the helper is an unmodified upstream binary that doesn't speak
`sd_listen_fds`, use proxy mode. container-init binds the public
port and proxies bytes to the helper's internal listener:

```ini
# /etc/container-init.d/legacy.socket
[Socket]
ListenStream=8080
ActivationMode=proxy
ProxyTarget=127.0.0.1:18080
Service=legacy.service

[Install]
WantedBy=sockets.target
```

```ini
# /etc/container-init.d/legacy.service
[Unit]
Requires=legacy.socket

[Service]
Type=simple
ExecStart=/usr/local/bin/legacy-server --listen 127.0.0.1:18080
Restart=on-failure
RestartSec=500ms
```

### 5. Socket-activated system D-Bus

For images that bundle a backend that needs the system bus
(polkitd, NetworkManager stub, custom hardware daemons):

```ini
# /etc/container-init.d/dbus-system.socket
[Socket]
ListenStream=/run/dbus/system_bus_socket
SocketUser=root
SocketMode=0666
ActivationMode=native
Service=dbus-system.service
```

```ini
# /etc/container-init.d/dbus-system.service
[Unit]
Requires=dbus-system.socket

[Service]
Type=simple
User=messagebus
Group=messagebus
ExecStartPre=/usr/bin/install -d -m 0755 /run/dbus
ExecStart=/usr/bin/dbus-daemon --system --nofork --nopidfile --syslog-only
Restart=on-failure
```

The backend service then `Requires=dbus-system.service` and
`After=dbus-system.service`. dbus-daemon natively understands
`sd_listen_fds` and consumes the listener fd container-init hands
over.

## Tracing

When `CONTAINER_INIT_TRACE=1`, container-init writes JSONL records to
`${CONTAINER_INIT_TRACE_FILE:-/tmp/container-init-trace.jsonl}`. Each
record has a stable shape: `t_start_ms`, `dt_ms` (per-phase elapsed,
NOT elapsed-from-boot), `phase`, optional `mem_snapshot`, and any
event-specific fields.

```sh
jq -s 'sort_by(.t_start_ms) | .[] | "\(.t_start_ms)ms \(.phase) \(.dt_ms)ms"' \
  /tmp/container-init-trace.jsonl
```

Set `CONTAINER_INIT_TRACE_LABELS="<unit>:<label>,<unit>:<label>"` to
capture a labelled `mem_snapshot` after specific units come up.

## License

Apache License 2.0 -- see [LICENSE](LICENSE).
