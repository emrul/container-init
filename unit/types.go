package unit

import (
	"os"
	"syscall"
	"time"
)

// Kind identifies the unit's section family.
type Kind int

const (
	KindService Kind = iota
	KindSocket
)

func (k Kind) String() string {
	switch k {
	case KindService:
		return "service"
	case KindSocket:
		return "socket"
	}
	return "unknown"
}

// ServiceType is the value of [Service] Type=.
type ServiceType int

const (
	TypeSimple ServiceType = iota
	TypeOneshot
	TypeForking
)

func (t ServiceType) String() string {
	switch t {
	case TypeSimple:
		return "simple"
	case TypeOneshot:
		return "oneshot"
	case TypeForking:
		return "forking"
	}
	return "unknown"
}

// RestartPolicy is the value of [Service] Restart=.
type RestartPolicy int

const (
	RestartNo RestartPolicy = iota
	RestartOnFailure
	RestartAlways
)

// ActivationMode selects how socket activation hands the public listener
// to the service. Native uses sd_listen_fds (LISTEN_PID/LISTEN_FDS,
// fd 3+); proxy keeps the listen fd in container-init and copies bytes
// to the service's private endpoint declared by ProxyTarget=.
type ActivationMode int

const (
	ActivationNative ActivationMode = iota
	ActivationProxy
)

func (a ActivationMode) String() string {
	switch a {
	case ActivationNative:
		return "native"
	case ActivationProxy:
		return "proxy"
	}
	return "unknown"
}

// Listener is a single ListenStream= or ListenDatagram= entry. AF_UNIX
// paths are absolute (start with "/"); everything else is parsed as a
// TCP/UDP port.
type Listener struct {
	Raw     string // verbatim Listen* value
	Network string // "tcp" | "tcp+udp" | "unix" | "unixgram"
	Address string // ":4902" or "/tmp/printer"
}

// Condition holds the evaluated outcome of every Condition* directive on
// a unit. The supervisor consults Skip; if true, the unit is logged as
// skipped and never started. Reason names the directive that triggered
// the skip.
type Condition struct {
	Skip   bool
	Reason string
}

// Unit is the parsed and validated representation of a single
// .service or .socket file. Directive values have already been
// env-expanded (${VAR} / ${VAR:-default}) and specifier-expanded
// (%n, %N, %H) at load time.
type Unit struct {
	Name string // e.g. "web.service"
	Path string // source path on disk
	Kind Kind

	// [Unit]
	Description             string
	After                   []string
	Before                  []string
	Requires                []string
	Wants                   []string
	ConditionPathExists     []string
	ConditionPathExistsGlob []string
	ConditionEnvironment    []string // each entry is "VAR=value" or "VAR"
	ConditionUser           []string // uid or user name, optional "!" prefix
	OnFailure               []string

	// [Service]
	Type         ServiceType
	ExecStart    []string   // argv (already shell-tokenised)
	ExecStartPre [][]string // ordered list of pre-start argvs
	ExecStop     [][]string // ordered list of stop argvs
	ExecStopPost [][]string // ordered list of post-stop argvs
	Restart      RestartPolicy
	RestartSec   time.Duration
	// Start rate limit: more than StartLimitBurst starts within
	// StartLimitIntervalSec fails the unit. Either one set alone gets
	// systemd's default for the other (5 / 10s); 0 in either disables
	// the limit, as does setting neither.
	StartLimitBurst        int
	StartLimitIntervalSec  time.Duration
	Environment            []string // "K=V"
	EnvironmentFile        []EnvFileRef
	ExitContainerOnFailure bool
	KillSignal             syscall.Signal // 0 = supervisor default (SIGTERM)
	TimeoutStartSec        time.Duration
	TimeoutStopSec         time.Duration
	RemainAfterExit        bool
	PIDFile                string

	// Privilege drop. The directive values have been env-expanded so a
	// unit using User=${APP_USER:-app} resolves at load time. The
	// supervisor wires these into SysProcAttr.Credential at spawn time.
	User             string
	Group            string
	WorkingDirectory string

	// [Socket]
	ListenStream   []Listener
	ListenDatagram []Listener
	Accept         bool // default false
	Service        string
	ActivationMode ActivationMode
	ProxyTarget    string
	SocketUser     string
	SocketGroup    string
	SocketMode     os.FileMode // 0 = "use kernel default"
	// Trigger rate limit: more than TriggerLimitBurst activations
	// within TriggerLimitIntervalSec fails the socket. Unlike the start
	// limit, a socket that sets neither gets systemd's defaults (20 /
	// 2s); 0 in either disables it.
	TriggerLimitBurst       int
	TriggerLimitIntervalSec time.Duration

	// [Install]
	WantedBy []string

	// Computed at load time.
	Condition Condition
}

// EnvFileRef is one EnvironmentFile= entry. IgnoreMissing is true when
// the directive value was prefixed with "-" (systemd's "ignore if
// absent" syntax).
type EnvFileRef struct {
	Path          string
	IgnoreMissing bool
}
