// Package trace emits boot-trace JSONL records: one line per phase or
// event, with per-phase dt_ms (elapsed within the phase, NOT
// elapsed-from-boot) and an optional mem_snapshot field. The line
// shape is stable so jq pipelines and dashboards can consume it
// directly.
package trace

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	envEnable = "CONTAINER_INIT_TRACE"
	envPath   = "CONTAINER_INIT_TRACE_FILE"
	defPath   = "/tmp/container-init-trace.jsonl"
)

// Tracer is safe for concurrent use; serialises writes through a mutex.
type Tracer struct {
	mu       sync.Mutex
	w        io.WriteCloser
	disabled bool
	bootMS   int64
}

// New opens the trace file if CONTAINER_INIT_TRACE is set; otherwise
// returns a no-op tracer. Emits a boot_start anchor record on
// success.
func New() *Tracer {
	t := &Tracer{bootMS: nowMS()}
	if os.Getenv(envEnable) != "1" {
		t.disabled = true
		return t
	}
	path := os.Getenv(envPath)
	if path == "" {
		path = defPath
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trace: open %s: %v\n", path, err)
		t.disabled = true
		return t
	}
	t.w = f
	t.emitRaw(map[string]any{
		"phase":      "boot_start",
		"t_start_ms": t.bootMS,
		"dt_ms":      0,
		"status":     "ok",
		"wall_utc":   time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	})
	return t
}

// Close flushes the underlying writer, if any.
func (t *Tracer) Close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.w != nil {
		_ = t.w.Close()
		t.w = nil
	}
}

// Phase is a begin/end pair — the End call emits a record with
// dt_ms = wallclock elapsed from Begin to End (matching the bash
// trace_phase_begin / trace_phase_end semantics).
type Phase struct {
	tracer  *Tracer
	name    string
	startMS int64
}

// Begin opens a phase. Returns nil when the tracer is disabled; End
// on a nil receiver is a no-op so callers don't need to check.
func (t *Tracer) Begin(name string) *Phase {
	if t == nil || t.disabled {
		return nil
	}
	return &Phase{tracer: t, name: name, startMS: nowMS()}
}

// End emits the phase record. Pass nil for fields when there are no
// extras to attach.
func (p *Phase) End(fields map[string]any) {
	if p == nil {
		return
	}
	p.endStatus("ok", fields)
}

// EndStatus is End with a non-"ok" status (e.g. "timeout", "error").
func (p *Phase) EndStatus(status string, fields map[string]any) {
	if p == nil {
		return
	}
	p.endStatus(status, fields)
}

func (p *Phase) endStatus(status string, fields map[string]any) {
	rec := map[string]any{
		"phase":      p.name,
		"t_start_ms": p.startMS,
		"dt_ms":      nowMS() - p.startMS,
		"status":     status,
	}
	for k, v := range fields {
		rec[k] = v
	}
	p.tracer.emitRaw(rec)
}

// Event is a point event (dt_ms=0). Used for spawn/exit notifications
// and other supervisor-internal milestones that don't have a "duration".
func (t *Tracer) Event(name string, fields map[string]any) {
	if t == nil || t.disabled {
		return
	}
	rec := map[string]any{
		"phase":      name,
		"t_start_ms": nowMS(),
		"dt_ms":      0,
		"status":     "ok",
	}
	for k, v := range fields {
		rec[k] = v
	}
	t.emitRaw(rec)
}

// MemSnapshot reads cgroup + per-process accounting and emits one
// record. Field names match the bash trace_mem_snapshot output exactly
// so dashboards keyed off cgroup_current_bytes / by_comm[].rss_kib
// work for both paths.
func (t *Tracer) MemSnapshot(label string) {
	if t == nil || t.disabled {
		return
	}
	snap := readMemSnapshot()
	rec := map[string]any{
		"phase":                "mem_snapshot",
		"label":                label,
		"t_start_ms":           nowMS(),
		"dt_ms":                0,
		"status":               "ok",
		"cgroup_current_bytes": snap.cgroupCurrent,
		"cgroup_peak_bytes":    snap.cgroupPeak,
		"cgroup_swap_bytes":    snap.cgroupSwap,
		"nproc":                snap.nproc,
		"rss_sum_bytes":        snap.rssSumBytes,
		"by_comm":              snap.byComm,
	}
	t.emitRaw(rec)
}

// ScheduleMemSnapshot emits a mem_snapshot after delay. Returns
// immediately; the snapshot fires on its own goroutine. Mirrors the
// bash trace_mem_steady_state_async helper.
func (t *Tracer) ScheduleMemSnapshot(label string, delay time.Duration) {
	if t == nil || t.disabled {
		return
	}
	go func() {
		time.Sleep(delay)
		t.MemSnapshot(label)
	}()
}

// emitRaw writes one JSONL record. Caller has already populated every
// field. Single point of escape-html control + write-mutex.
func (t *Tracer) emitRaw(rec map[string]any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.w == nil {
		return
	}
	enc := json.NewEncoder(t.w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(rec)
}

func nowMS() int64 { return time.Now().UnixNano() / int64(time.Millisecond) }

// memSnap holds the raw figures collected by readMemSnapshot. We keep
// it as an internal struct so the on-disk JSON shape lives in one
// place (MemSnapshot above).
type memSnap struct {
	cgroupCurrent any
	cgroupPeak    any
	cgroupSwap    any
	nproc         int
	rssSumBytes   int64
	byComm        []map[string]any
}

// readMemSnapshot mirrors the bash trace_mem_snapshot reader. Falls
// back to nil for any field it can't read so the JSON record's shape
// stays stable.
func readMemSnapshot() memSnap {
	s := memSnap{
		cgroupCurrent: nil,
		cgroupPeak:    nil,
		cgroupSwap:    nil,
		byComm:        []map[string]any{},
	}
	if v, ok := readUint64("/sys/fs/cgroup/memory.current"); ok {
		s.cgroupCurrent = v
	} else if v, ok := readUint64("/sys/fs/cgroup/memory/memory.usage_in_bytes"); ok {
		s.cgroupCurrent = v
	}
	if v, ok := readUint64("/sys/fs/cgroup/memory.peak"); ok {
		s.cgroupPeak = v
	} else if v, ok := readUint64("/sys/fs/cgroup/memory/memory.max_usage_in_bytes"); ok {
		s.cgroupPeak = v
	}
	if v, ok := readUint64("/sys/fs/cgroup/memory.swap.current"); ok {
		s.cgroupSwap = v
	}

	pageSize := int64(syscall.Getpagesize())
	commRSS := map[string]int64{}
	procEntries, _ := os.ReadDir("/proc")
	for _, e := range procEntries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		statm, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
		if err != nil {
			continue
		}
		fields := strings.Fields(string(statm))
		if len(fields) < 2 {
			continue
		}
		rssPages, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		s.rssSumBytes += rssPages * pageSize
		s.nproc++
		commBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err != nil {
			continue
		}
		comm := strings.TrimSpace(string(commBytes))
		if comm == "" {
			continue
		}
		commRSS[comm] += rssPages * pageSize / 1024
	}
	keys := make([]string, 0, len(commRSS))
	for k := range commRSS {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s.byComm = append(s.byComm, map[string]any{"comm": k, "rss_kib": commRSS[k]})
	}
	return s
}

func readUint64(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// PhaseFromUnitName returns the canonical "<unit-base>_invoke" phase
// name for a service unit (e.g. "web.service" → "web_invoke").
func PhaseFromUnitName(unit string) string {
	base := unit
	if i := strings.LastIndex(unit, "."); i > 0 {
		base = unit[:i]
	}
	// Dashes pass through unchanged: "audio-out-ws.service" yields
	// "audio-out-ws_invoke".
	return base + "_invoke"
}

// PostSpawnLabel returns the mem_snapshot label requested for unit by
// the CONTAINER_INIT_TRACE_LABELS env var, or "" when none. Format:
// "<unit1>:<label1>,<unit2>:<label2>". This is image policy: the
// image author sets the value in the entrypoint so labelled
// snapshots fire at the points relevant to that image (e.g.
// post_web, post_services).
func PostSpawnLabel(envValue, unit string) string {
	if envValue == "" {
		return ""
	}
	for _, pair := range strings.Split(envValue, ",") {
		pair = strings.TrimSpace(pair)
		c := strings.IndexByte(pair, ':')
		if c <= 0 {
			continue
		}
		if pair[:c] == unit {
			return pair[c+1:]
		}
	}
	return ""
}

// Filename returns the path the tracer is writing to (for diagnostic
// log lines from main).
func (t *Tracer) Filename() string {
	if t == nil || t.disabled {
		return ""
	}
	if f, ok := t.w.(*os.File); ok {
		return filepath.Clean(f.Name())
	}
	return ""
}

// Goarch is exported for completeness so callers building cross-arch
// integration tests don't need to import runtime themselves.
func Goarch() string { return runtime.GOARCH }
