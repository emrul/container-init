# container-init

A small PID 1 supervisor for container images. Reads a documented
subset of systemd unit files, supervises services, and provides
socket activation in two modes (native via `sd_listen_fds`, and proxy
for unmodified upstream binaries).

The binary is generic; image-specific behaviour lives in the unit
files an image author drops at `/etc/container-init/units/` and
`/etc/container-init.d/`.

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

`unit/` is the only exported package — downstream images can import it
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

1. **`/etc/container-init/units/`** — core unit files installed by the
   base image.
2. **`/etc/container-init.d/`** — drop-ins shipped by derived images
   that layer on top of the base. Drop-ins go through the same parser
   and validator as core; they can reference core units via `After=`
   / `Requires=` / `OnFailure=`.

Both paths are configurable via the `--units` and `--drop-in` flags.

The `--strict-units` flag promotes any parser warning (unknown
directive / section, unsupported value form) into a fatal load error
— useful in CI to catch typos before they ship.

The `--validate` flag loads + parses units, prints a summary, and
exits without supervising. Combined with `--strict-units`, this is a
build-time sanity check.

## Extension point — `/etc/container-init.d/`

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

A drop-in with any *other* name is **additive** — added to the unit
graph, parsed, supervised, and shut down alongside the core set.

### Naming conventions

- For an *intentional* override, name the drop-in identically to the
  core unit you want to replace. There is no namespacing; full
  filename match is the override key.
- Recommended convention for additive units:
  `<image-name>-<service>.service` —
  e.g. `chrome-launcher.service`, `vscode-server.service`.

### Validation

Drop-ins go through the same supported-directive validator as core
units. Unsupported directives produce a parse-time warning naming the
file + section + directive (default) or fail-fast (`--strict-units`).

### Restart, conditions, lifecycle

Drop-ins participate in the supervisor's full lifecycle:

- **Restart= / RestartSec= / StartLimitBurst= / StartLimitIntervalSec=** —
  per-unit restart policy and rate limiting.
- **ConditionPathExists= / ConditionPathExistsGlob= / ConditionEnvironment=** —
  unit is loaded but skipped at boot when conditions are unmet.
- **OnFailure=** — invoke a sibling oneshot when this unit's restart
  policy is exhausted; chains across the core/drop-in boundary
  identically.
- **ExitContainerOnFailure=true** — fail-secure: take the whole
  container down via reverse shutdown when this unit's failure path
  is reached. Useful for compliance gates an image author owns.
- **User= / Group= / WorkingDirectory=** — privilege drop. Accepts
  the `${VAR:-default}` env-expansion form so per-image overrides
  flow through automatically.

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

[Service]
Type=simple
User=${APP_USER:-app}
WorkingDirectory=${APP_HOME:-/home/app}
ExecStart=/usr/local/bin/myimage-app --listen 127.0.0.1:5000
Restart=on-failure
RestartSec=2s
StartLimitBurst=5
StartLimitIntervalSec=60s
```

### 3. Socket-activated helper, native mode

When the helper consumes `LISTEN_PID` / `LISTEN_FDS`, use native
activation. container-init binds the public port and hands the
listener fd over on first connect:

```ini
# /etc/container-init.d/myhelper.socket
[Unit]
Description=My helper — public listener

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

Apache License 2.0 — see [LICENSE](LICENSE).
