package unit

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	gounit "github.com/coreos/go-systemd/v22/unit"
)

// Lookup is the env-var resolver passed to Expand. The default lookup
// (OSLookup) reads container-init's own environment.
type Lookup func(string) (string, bool)

// OSLookup is the production Lookup — reads from os.Environ().
func OSLookup(k string) (string, bool) { return os.LookupEnv(k) }

// Options configures unit loading. Zero value is usable: defaults to
// OSLookup, non-strict.
type Options struct {
	// Lookup resolves ${VAR} expansion in directive values.
	Lookup Lookup
	// Strict converts unknown-directive and unknown-section warnings
	// into errors. Default false: parsing continues with warnings.
	Strict bool
}

// Warning is a non-fatal parse note. Surfaced for the operator and
// promoted to errors in strict mode.
type Warning struct {
	Path      string // unit-file path
	Section   string // "Unit" / "Service" / "Socket" / "Install" / ""
	Directive string // directive name, or "" for whole-section warnings
	Message   string
}

func (w Warning) String() string {
	loc := filepath.Base(w.Path)
	switch {
	case w.Section != "" && w.Directive != "":
		return fmt.Sprintf("%s [%s] %s: %s", loc, w.Section, w.Directive, w.Message)
	case w.Section != "":
		return fmt.Sprintf("%s [%s]: %s", loc, w.Section, w.Message)
	}
	return fmt.Sprintf("%s: %s", loc, w.Message)
}

// Override records one drop-in that replaced a core unit by name.
// The loader emits one entry per (Name, BasePath -> OverridePath)
// pairing so main can log the overlay decision at boot.
type Override struct {
	Name         string
	BasePath     string
	OverridePath string
}

// LoadOverlay loads units from each directory in dirs, overlaying
// later directories onto earlier ones by unit name. Drop-ins thus
// override core units by name; non-overlapping drop-ins are
// additive. Each replacement is recorded in the returned overrides
// slice so callers can log "overridden by <drop-in path>" at boot.
//
// Empty / missing directories are skipped silently — operators who
// don't ship a /etc/container-init.d/ shouldn't see a load error.
func LoadOverlay(dirs []string, opts Options) ([]*Unit, []Warning, []Override, error) {
	if opts.Lookup == nil {
		opts.Lookup = OSLookup
	}
	byName := map[string]*Unit{}
	var (
		warnings  []Warning
		overrides []Override
		errs      []string
	)
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if _, err := os.Stat(d); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, fmt.Sprintf("%s: %v", d, err))
			continue
		}
		us, ws, err := LoadDir(d, opts)
		warnings = append(warnings, ws...)
		if err != nil {
			errs = append(errs, err.Error())
			// continue — still apply units that loaded
		}
		for _, u := range us {
			if prev, ok := byName[u.Name]; ok {
				overrides = append(overrides, Override{
					Name:         u.Name,
					BasePath:     prev.Path,
					OverridePath: u.Path,
				})
			}
			byName[u.Name] = u
		}
	}
	out := make([]*Unit, 0, len(byName))
	for _, u := range byName {
		out = append(out, u)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(errs) > 0 {
		return out, warnings, overrides, fmt.Errorf("unit load errors:\n  %s", strings.Join(errs, "\n  "))
	}
	if opts.Strict && len(warnings) > 0 {
		var msgs []string
		for _, w := range warnings {
			msgs = append(msgs, w.String())
		}
		return out, warnings, overrides, fmt.Errorf("strict-units: %d warning(s):\n  %s", len(warnings), strings.Join(msgs, "\n  "))
	}
	return out, warnings, overrides, nil
}

// LoadDir parses every *.service / *.socket in dir. Returns the units
// in deterministic load order (filename sort), the collected warnings,
// and a non-nil error if any file failed fatally — or, in strict mode,
// if any warnings were emitted.
func LoadDir(dir string, opts Options) ([]*Unit, []Warning, error) {
	if opts.Lookup == nil {
		opts.Lookup = OSLookup
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read unit dir %q: %w", dir, err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".service") && !strings.HasSuffix(name, ".socket") {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	sort.Strings(paths)

	var (
		units    []*Unit
		warnings []Warning
		errs     []string
	)
	for _, p := range paths {
		u, ws, err := LoadFile(p, opts)
		warnings = append(warnings, ws...)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", filepath.Base(p), err))
			continue
		}
		units = append(units, u)
	}
	if len(errs) > 0 {
		return units, warnings, fmt.Errorf("unit load errors:\n  %s", strings.Join(errs, "\n  "))
	}
	if opts.Strict && len(warnings) > 0 {
		var msgs []string
		for _, w := range warnings {
			msgs = append(msgs, w.String())
		}
		return units, warnings, fmt.Errorf("strict-units: %d warning(s):\n  %s", len(warnings), strings.Join(msgs, "\n  "))
	}
	return units, warnings, nil
}

// LoadFile parses a single unit file.
func LoadFile(path string, opts Options) (*Unit, []Warning, error) {
	if opts.Lookup == nil {
		opts.Lookup = OSLookup
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	rawOpts, err := gounit.Deserialize(f)
	if err != nil {
		return nil, nil, fmt.Errorf("parse: %w", err)
	}
	name := filepath.Base(path)
	u := &Unit{Name: name, Path: path}
	switch {
	case strings.HasSuffix(name, ".service"):
		u.Kind = KindService
		u.Type = TypeSimple
		u.Restart = RestartNo
		u.RestartSec = 100 * time.Millisecond
	case strings.HasSuffix(name, ".socket"):
		u.Kind = KindSocket
		u.ActivationMode = ActivationNative
	default:
		return nil, nil, fmt.Errorf("unsupported unit suffix")
	}

	specs := UnitSpecifiers(name)
	l := loader{path: path, opts: opts}
	for _, opt := range rawOpts {
		val := Expand(opt.Value, opts.Lookup)
		val = ExpandSpecifiers(val, specs)
		if err := l.applyDirective(u, opt.Section, opt.Name, val); err != nil {
			return nil, l.warnings, err
		}
	}
	if err := validate(u); err != nil {
		return nil, l.warnings, err
	}
	if opts.Strict && len(l.warnings) > 0 {
		return u, l.warnings, fmt.Errorf("strict-units: %d warning(s) on %s", len(l.warnings), name)
	}
	evaluateConditions(u, opts.Lookup)
	return u, l.warnings, nil
}

// loader carries the per-file parser state (so warnings collect
// without leaking into the package-level surface).
type loader struct {
	path     string
	opts     Options
	warnings []Warning
}

func (l *loader) warn(section, directive, msg string) {
	l.warnings = append(l.warnings, Warning{
		Path:      l.path,
		Section:   section,
		Directive: directive,
		Message:   msg,
	})
}

// applyDirective routes a single (section, name, value) tuple onto the
// Unit. Unknown sections / directives produce a warning rather than an
// error so the production unit set can layer on top of an older
// container-init binary; strict mode (Options.Strict) promotes
// warnings to errors at the LoadFile boundary.
func (l *loader) applyDirective(u *Unit, section, name, value string) error {
	switch section {
	case "Unit":
		return l.applyUnitSection(u, name, value)
	case "Service":
		if u.Kind != KindService {
			l.warn(section, name, "[Service] directive in non-service unit (ignored)")
			return nil
		}
		return l.applyServiceSection(u, name, value)
	case "Socket":
		if u.Kind != KindSocket {
			l.warn(section, name, "[Socket] directive in non-socket unit (ignored)")
			return nil
		}
		return l.applySocketSection(u, name, value)
	case "Install":
		return l.applyInstallSection(u, name, value)
	default:
		l.warn(section, name, fmt.Sprintf("unknown section [%s] (ignored)", section))
		return nil
	}
}

func (l *loader) applyUnitSection(u *Unit, name, value string) error {
	switch name {
	case "Description":
		u.Description = value
	case "After":
		u.After = append(u.After, splitWords(value)...)
	case "Before":
		u.Before = append(u.Before, splitWords(value)...)
	case "Requires":
		u.Requires = append(u.Requires, splitWords(value)...)
	case "Wants":
		u.Wants = append(u.Wants, splitWords(value)...)
	case "ConditionPathExists":
		u.ConditionPathExists = append(u.ConditionPathExists, value)
	case "ConditionPathExistsGlob":
		u.ConditionPathExistsGlob = append(u.ConditionPathExistsGlob, value)
	case "ConditionEnvironment":
		u.ConditionEnvironment = append(u.ConditionEnvironment, value)
	case "OnFailure":
		u.OnFailure = append(u.OnFailure, splitWords(value)...)
	default:
		l.warn("Unit", name, "directive not in supported subset (ignored)")
	}
	return nil
}

func (l *loader) applyServiceSection(u *Unit, name, value string) error {
	switch name {
	case "Type":
		switch value {
		case "simple":
			u.Type = TypeSimple
		case "oneshot":
			u.Type = TypeOneshot
		case "forking":
			u.Type = TypeForking
		default:
			return fmt.Errorf("[Service] Type=%q not supported (simple|oneshot|forking)", value)
		}
	case "ExecStart":
		argv, err := splitExec(value)
		if err != nil {
			return fmt.Errorf("ExecStart: %w", err)
		}
		u.ExecStart = argv
	case "ExecStartPre":
		argv, err := splitExec(value)
		if err != nil {
			return fmt.Errorf("ExecStartPre: %w", err)
		}
		u.ExecStartPre = append(u.ExecStartPre, argv)
	case "ExecStop":
		argv, err := splitExec(value)
		if err != nil {
			return fmt.Errorf("ExecStop: %w", err)
		}
		u.ExecStop = append(u.ExecStop, argv)
	case "ExecStopPost":
		argv, err := splitExec(value)
		if err != nil {
			return fmt.Errorf("ExecStopPost: %w", err)
		}
		u.ExecStopPost = append(u.ExecStopPost, argv)
	case "Restart":
		switch value {
		case "no":
			u.Restart = RestartNo
		case "on-failure":
			u.Restart = RestartOnFailure
		case "always":
			u.Restart = RestartAlways
		default:
			return fmt.Errorf("[Service] Restart=%q not supported (no|on-failure|always)", value)
		}
	case "RestartSec":
		d, err := parseDuration(value)
		if err != nil {
			return fmt.Errorf("RestartSec: %w", err)
		}
		u.RestartSec = d
	case "StartLimitBurst":
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("StartLimitBurst: %w", err)
		}
		u.StartLimitBurst = n
	case "StartLimitIntervalSec":
		d, err := parseDuration(value)
		if err != nil {
			return fmt.Errorf("StartLimitIntervalSec: %w", err)
		}
		u.StartLimitIntervalSec = d
	case "Environment":
		// gounit's deserialiser hands us one string with quoting
		// already collapsed; split on whitespace to get K=V pairs.
		u.Environment = append(u.Environment, splitWords(value)...)
	case "EnvironmentFile":
		v := strings.TrimSpace(value)
		ref := EnvFileRef{Path: v}
		if strings.HasPrefix(v, "-") {
			ref.IgnoreMissing = true
			ref.Path = strings.TrimPrefix(v, "-")
		}
		u.EnvironmentFile = append(u.EnvironmentFile, ref)
	case "ExitContainerOnFailure":
		b, err := parseBool(value)
		if err != nil {
			return fmt.Errorf("ExitContainerOnFailure: %w", err)
		}
		u.ExitContainerOnFailure = b
	case "KillSignal":
		sig, err := parseSignal(value)
		if err != nil {
			return fmt.Errorf("KillSignal: %w", err)
		}
		u.KillSignal = sig
	case "TimeoutStartSec":
		d, err := parseDuration(value)
		if err != nil {
			return fmt.Errorf("TimeoutStartSec: %w", err)
		}
		u.TimeoutStartSec = d
	case "TimeoutStopSec":
		d, err := parseDuration(value)
		if err != nil {
			return fmt.Errorf("TimeoutStopSec: %w", err)
		}
		u.TimeoutStopSec = d
	case "RemainAfterExit":
		b, err := parseBool(value)
		if err != nil {
			return fmt.Errorf("RemainAfterExit: %w", err)
		}
		u.RemainAfterExit = b
	case "PIDFile":
		u.PIDFile = strings.TrimSpace(value)
	case "User":
		u.User = strings.TrimSpace(value)
	case "Group":
		u.Group = strings.TrimSpace(value)
	case "WorkingDirectory":
		u.WorkingDirectory = strings.TrimSpace(value)
	default:
		l.warn("Service", name, "directive not in supported subset (ignored)")
	}
	return nil
}

func (l *loader) applySocketSection(u *Unit, name, value string) error {
	switch name {
	case "ListenStream":
		ln, err := parseListener(value, "tcp", "unix")
		if err != nil {
			return fmt.Errorf("ListenStream=%q: %w", value, err)
		}
		u.ListenStream = append(u.ListenStream, ln)
	case "ListenDatagram":
		ln, err := parseListener(value, "udp", "unixgram")
		if err != nil {
			return fmt.Errorf("ListenDatagram=%q: %w", value, err)
		}
		u.ListenDatagram = append(u.ListenDatagram, ln)
	case "Accept":
		b, err := parseBool(value)
		if err != nil {
			return fmt.Errorf("Accept: %w", err)
		}
		if b {
			return fmt.Errorf("[Socket] Accept=yes not supported (only fd-passing on first connect)")
		}
		u.Accept = false
	case "Service":
		u.Service = strings.TrimSpace(value)
	case "ActivationMode":
		switch value {
		case "native":
			u.ActivationMode = ActivationNative
		case "proxy":
			u.ActivationMode = ActivationProxy
		default:
			return fmt.Errorf("[Socket] ActivationMode=%q must be native|proxy", value)
		}
	case "ProxyTarget":
		u.ProxyTarget = strings.TrimSpace(value)
	case "SocketUser":
		u.SocketUser = strings.TrimSpace(value)
	case "SocketGroup":
		u.SocketGroup = strings.TrimSpace(value)
	case "SocketMode":
		m, err := parseFileMode(value)
		if err != nil {
			return fmt.Errorf("SocketMode: %w", err)
		}
		u.SocketMode = m
	default:
		l.warn("Socket", name, "directive not in supported subset (ignored)")
	}
	return nil
}

func (l *loader) applyInstallSection(u *Unit, name, value string) error {
	switch name {
	case "WantedBy":
		u.WantedBy = append(u.WantedBy, splitWords(value)...)
	default:
		l.warn("Install", name, "directive not in supported subset (ignored)")
	}
	return nil
}

// splitWords splits on systemd whitespace. The gounit deserialiser
// already handles line-continuations and surrounding quotes; what
// reaches us is a single logical value that may still contain multiple
// words separated by spaces.
func splitWords(value string) []string {
	return strings.Fields(value)
}

// splitExec is the simple shell tokeniser for ExecStart=. Supports
// double-quoted arguments containing spaces. Backslash escapes are
// not interpreted; that's a Phase 4+ concern when the subset widens.
func splitExec(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("empty ExecStart")
	}
	var argv []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case c == ' ' && !inQuote:
			if cur.Len() > 0 {
				argv = append(argv, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quote")
	}
	if cur.Len() > 0 {
		argv = append(argv, cur.String())
	}
	return argv, nil
}

// parseDuration accepts "100ms", "5s", "1500", or "1.5s". Bare numbers
// are interpreted as seconds, matching systemd.
func parseDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if d, err := time.ParseDuration(value); err == nil {
		return d, nil
	}
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		return time.Duration(f * float64(time.Second)), nil
	}
	return 0, fmt.Errorf("not a duration: %q", value)
}

func parseBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "yes", "true", "on":
		return true, nil
	case "0", "no", "false", "off":
		return false, nil
	}
	return false, fmt.Errorf("not a boolean: %q", value)
}

// parseListener turns a Listen{Stream,Datagram}= value into a
// (network, address) pair. Absolute paths are AF_UNIX; everything else
// is parsed as a port. tcpNet / unixNet pick the network family for
// the two forms.
func parseListener(value, tcpNet, unixNet string) (Listener, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return Listener{}, fmt.Errorf("empty")
	}
	if strings.HasPrefix(v, "/") {
		return Listener{Raw: value, Network: unixNet, Address: v}, nil
	}
	if _, err := strconv.Atoi(v); err == nil {
		return Listener{Raw: value, Network: tcpNet, Address: ":" + v}, nil
	}
	if strings.Contains(v, ":") {
		return Listener{Raw: value, Network: tcpNet, Address: v}, nil
	}
	return Listener{}, fmt.Errorf("not a port or AF_UNIX path: %q", value)
}

// parseFileMode parses an octal SocketMode= value. "0660", "660",
// "0o660" all accepted; the leading "0o" is trimmed.
func parseFileMode(value string) (os.FileMode, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0, fmt.Errorf("empty")
	}
	v = strings.TrimPrefix(v, "0o")
	n, err := strconv.ParseUint(v, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("not octal: %q", value)
	}
	return os.FileMode(n) & os.ModePerm, nil
}

// killSignals maps directive-form signal names to the syscall constant.
// systemd uses the SIG-prefixed form everywhere; numeric form is also
// accepted for parity with documented unit-file behaviour.
var killSignals = map[string]syscall.Signal{
	"SIGHUP":  syscall.SIGHUP,
	"SIGINT":  syscall.SIGINT,
	"SIGQUIT": syscall.SIGQUIT,
	"SIGKILL": syscall.SIGKILL,
	"SIGTERM": syscall.SIGTERM,
	"SIGUSR1": syscall.SIGUSR1,
	"SIGUSR2": syscall.SIGUSR2,
	"SIGABRT": syscall.SIGABRT,
}

func parseSignal(value string) (syscall.Signal, error) {
	v := strings.ToUpper(strings.TrimSpace(value))
	if !strings.HasPrefix(v, "SIG") {
		v = "SIG" + v
	}
	if sig, ok := killSignals[v]; ok {
		return sig, nil
	}
	if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n > 0 {
		return syscall.Signal(n), nil
	}
	return 0, fmt.Errorf("not a signal: %q", value)
}
