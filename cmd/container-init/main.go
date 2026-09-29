// container-init is a PID 1 supervisor for container images.
//
// It reads a documented subset of systemd unit files from a directory
// (default /etc/container-init/units), supervises services, and
// provides socket activation in two modes (native + proxy).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/execwrap"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/internal/supervisor"
	"github.com/emrul/container-init/internal/systemd1shim"
	"github.com/emrul/container-init/internal/trace"
	"github.com/emrul/container-init/unit"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// A service spawn re-executes this binary as its first step; that
	// never returns. See execwrap.
	execwrap.Main()

	dir := flag.String("units", "/etc/container-init/units", "directory containing core .service / .socket files")
	dropIn := flag.String("drop-in", "/etc/container-init.d", "directory containing image-author drop-ins (override core by name)")
	strict := flag.Bool("strict-units", false, "fail fast on any parser warning (unknown directive / section)")
	validate := flag.Bool("validate", false, "load + parse units, print summary, exit without supervising (build-time sanity)")
	systemd1Shim := flag.String("systemd1-shim", "", "comma-separated D-Bus addresses to register org.freedesktop.systemd1 on (e.g. \"user:1000\" or \"user:env:KASM_OS_UID\"); empty disables")
	stopTimeout := flag.Duration("stop-timeout", 8*time.Second, "bound on the whole reverse shutdown; units still running after it are killed (keep it under docker stop's timeout, 10s by default)")
	versionFlag := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Println("container-init", version)
		os.Exit(0)
	}

	log.SetFlags(0)
	log.SetPrefix("container-init: ")

	// Records are written by the tracer's own goroutine, and os.Exit
	// runs no defers: every exit below flushes it explicitly. Close is
	// bounded -- by reverse shutdown's deadline once that has begun,
	// otherwise by trace.CloseWait.
	tracer := trace.New()
	exitWith := func(code int) {
		tracer.Close()
		os.Exit(code)
	}
	tracer.MemSnapshot("boot")

	units, warnings, overrides, err := unit.LoadOverlay([]string{*dir, *dropIn}, unit.Options{Lookup: unit.OSLookup, Strict: *strict})
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "container-init: warning: %s\n", w)
	}
	for _, o := range overrides {
		log.Printf("drop-in override: %s replaces %s with %s", o.Name, o.BasePath, o.OverridePath)
		tracer.Event("unit_overridden", map[string]any{
			"unit":          o.Name,
			"base_path":     o.BasePath,
			"override_path": o.OverridePath,
		})
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "container-init: load units: %v\n", err)
		exitWith(64)
	}
	tracer.Event("units_loaded", map[string]any{
		"count":     len(units),
		"warnings":  len(warnings),
		"overrides": len(overrides),
	})

	if *validate {
		fmt.Fprintf(os.Stdout,
			"container-init validate: units=%d warnings=%d overrides=%d strict=%v\n",
			len(units), len(warnings), len(overrides), *strict)
		// The ordering check startup makes: a cycle fails the boot
		// with 64, so it fails validation the same way, strict or not.
		if err := supervisor.CheckOrder(units); err != nil {
			fmt.Fprintf(os.Stderr, "container-init: validate: %v\n", err)
			exitWith(64)
		}
		// Strict mode already exits non-zero above on any warning; in
		// non-strict mode we still want a non-zero exit if any warning
		// was raised, so build pipelines catch directive drift.
		if len(warnings) > 0 {
			exitWith(1)
		}
		exitWith(0)
	}

	dispatcherStop := make(chan struct{})
	dispatcher := pid1.NewDispatcher()
	dispatcher.Start(dispatcherStop)

	cg := cgroup.New()
	if cg.Available() {
		log.Printf("cgroup-v2 base: %s", cg.Base())
		tracer.Event("cgroup_init", map[string]any{"available": true, "base": cg.Base()})
	} else {
		log.Printf("cgroup-v2 unavailable (%v); falling back to PGID kill path", cg.Err())
		tracer.Event("cgroup_init", map[string]any{"available": false, "err": fmt.Sprint(cg.Err())})
	}

	sup, err := supervisor.New(units, tracer, dispatcher, cg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "container-init: supervisor: %v\n", err)
		close(dispatcherStop)
		exitWith(64)
	}
	sup.SetStopTimeout(*stopTimeout)

	// Compatibility shim for clients that probe systemd by spawning
	// systemd-run (notably Ptyxis). Off by default; opt-in via flag.
	// os.Exit at the bottom of main doesn't run defers, so the shim
	// is closed explicitly after sup.Run() returns.
	var shim *systemd1shim.Shim
	if *systemd1Shim != "" {
		addrs := strings.Split(*systemd1Shim, ",")
		s, err := systemd1shim.Open(context.Background(), addrs, func(format string, args ...any) {
			log.Printf(format, args...)
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "container-init: systemd1-shim: %v\n", err)
		} else {
			shim = s
			tracer.Event("systemd1_shim_started", map[string]any{"addresses": addrs})
		}
	}

	// SIGTERM / SIGINT → reverse shutdown.
	go pid1.ForwardSignals(func(sig os.Signal) {
		log.Printf("received %v, beginning reverse shutdown", sig)
		tracer.Event("signal_received", map[string]any{"sig": sig.String()})
		sup.Stop()
	}, sup.Done())

	tracer.Event("supervisor_start", nil)
	exit := sup.Run()
	if shim != nil {
		shim.Close()
	}
	close(dispatcherStop)
	<-dispatcher.Done()
	tracer.Event("boot_done", map[string]any{"exit": exit})
	exitWith(exit)
}
