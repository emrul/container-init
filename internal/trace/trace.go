// Package trace emits boot-trace JSONL records: one line per phase or
// event, with per-phase dt_ms (elapsed within the phase, NOT
// elapsed-from-boot) and an optional mem_snapshot field. The line
// shape is stable so jq pipelines and dashboards can consume it
// directly.
package trace

import (
	"bytes"
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
	"sync/atomic"
	"syscall"
	"time"
)

const (
	envEnable = "CONTAINER_INIT_TRACE"
	envPath   = "CONTAINER_INIT_TRACE_FILE"
	defPath   = "/tmp/container-init-trace.jsonl"
)

// Queue and CloseWait bound what tracing can cost the supervisor.
const (
	// Queue is how many encoded records may wait for the writer. A
	// record emitted while it is full is dropped and counted.
	Queue = 4096
	// CloseWait bounds Close's flush when no deadline has been set
	// with SetDeadline.
	CloseWait = time.Second
)

// Tracer is safe for concurrent use. Emitting a record never waits
// for I/O: records are encoded by the caller and queued for a
// dedicated writer goroutine, so a slow or stalled destination (a
// full pipe, a hung disk) costs dropped records, not a stuck
// supervisor. Drops are reported in-stream by trace_dropped records
// once the writer makes progress.
type Tracer struct {
	mu      sync.RWMutex // guards closed against sends on q
	closed  bool
	q       chan []byte
	done    chan struct{} // closed when the writer has finished
	w       io.WriteCloser
	path    string
	dropped atomic.Uint64
	// dmu guards deadline and moved; moved is closed, and replaced,
	// whenever SetDeadline changes the deadline, waking every Close
	// in progress to wait for the new one.
	dmu      sync.Mutex
	deadline time.Time // zero: none set
	moved    chan struct{}
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
	t.w, t.path = f, filepath.Clean(path)
	t.q = make(chan []byte, Queue)
	t.moved = make(chan struct{})
	t.done = make(chan struct{})
	go t.write()
	t.emitRaw(map[string]any{
		"phase":      "boot_start",
		"t_start_ms": t.bootMS,
		"dt_ms":      0,
		"status":     "ok",
		"wall_utc":   time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	})
	return t
}

// write is the writer goroutine: it drains the queue until Close,
// reporting drops as it catches up, then closes the file.
func (t *Tracer) write() {
	defer close(t.done)
	defer t.w.Close()
	var reported uint64
	report := func() {
		if n := t.dropped.Load(); n > reported {
			rec, _ := encode(map[string]any{
				"phase":      "trace_dropped",
				"t_start_ms": nowMS(),
				"dt_ms":      0,
				"status":     "error",
				"dropped":    n - reported,
				"total":      n,
			})
			reported = n
			_, _ = t.w.Write(rec)
		}
	}
	for rec := range t.q {
		_, _ = t.w.Write(rec)
		report()
	}
	report()
}

// SetDeadline bounds every Close's flush at d -- later ones, and any
// already waiting, which then wait for d instead. Reverse shutdown
// sets its own deadline here, so the final flush fits within it rather
// than adding to it.
func (t *Tracer) SetDeadline(d time.Time) {
	if t == nil || t.disabled {
		return
	}
	t.dmu.Lock()
	defer t.dmu.Unlock()
	t.deadline = d
	close(t.moved)
	t.moved = make(chan struct{})
}

// Close stops accepting records and waits for the writer to flush
// what is queued -- until the SetDeadline deadline, which may change
// while it waits, or for CloseWait from the call when none is set. A
// writer still stuck then is abandoned and Close returns anyway. Any
// number of calls, deferred or not, each wait at most that long.
func (t *Tracer) Close() {
	if t == nil || t.disabled {
		return
	}
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		close(t.q)
	}
	t.mu.Unlock()
	fallback := time.Now().Add(CloseWait)
	for {
		t.dmu.Lock()
		d, moved := t.deadline, t.moved
		t.dmu.Unlock()
		if d.IsZero() {
			d = fallback
		}
		timer := time.NewTimer(time.Until(d))
		select {
		case <-t.done:
			timer.Stop()
			return
		case <-timer.C:
			return
		case <-moved:
			timer.Stop()
		}
	}
}

// Dropped reports how many records were dropped because the queue was
// full.
func (t *Tracer) Dropped() uint64 {
	if t == nil {
		return 0
	}
	return t.dropped.Load()
}

// Phase is a begin/end pair: End emits a record with dt_ms, the
// wall-clock time elapsed from Begin to End.
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

// Event emits a point event (dt_ms=0), for milestones without a
// duration such as spawns and exits.
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
// record. Field names match the bash trace_mem_snapshot output, so
// dashboards keyed off cgroup_current_bytes / by_comm[].rss_kib read
// either.
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

// ScheduleMemSnapshot emits a mem_snapshot after delay, from its own
// goroutine; it returns immediately.
func (t *Tracer) ScheduleMemSnapshot(label string, delay time.Duration) {
	if t == nil || t.disabled {
		return
	}
	go func() {
		time.Sleep(delay)
		t.MemSnapshot(label)
	}()
}

// emitRaw queues one JSONL record for the writer. Caller has already
// populated every field. It never blocks: a full queue drops the
// record, and a closed tracer discards it.
func (t *Tracer) emitRaw(rec map[string]any) {
	if t.disabled {
		return
	}
	b, err := encode(rec)
	if err != nil {
		return
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.closed {
		return
	}
	select {
	case t.q <- b:
	default:
		t.dropped.Add(1)
	}
}

// encode is the single point of escape-html control.
func encode(rec map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func nowMS() int64 { return time.Now().UnixNano() / int64(time.Millisecond) }

// memSnap holds the raw figures collected by readMemSnapshot; the JSON
// shape lives in one place, MemSnapshot.
type memSnap struct {
	cgroupCurrent any
	cgroupPeak    any
	cgroupSwap    any
	nproc         int
	rssSumBytes   int64
	byComm        []map[string]any
}

// readMemSnapshot reads the figures bash's trace_mem_snapshot reports.
// A field it cannot read is nil, so the record's shape stays stable.
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
	return t.path
}

// Goarch returns runtime.GOARCH, for cross-arch integration tests.
func Goarch() string { return runtime.GOARCH }
