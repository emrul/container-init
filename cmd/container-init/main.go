// container-init is a PID 1 supervisor for container images.
//
// It reads a documented subset of systemd unit files from a directory
// (default /etc/container-init/units), supervises services, and
// provides socket activation in two modes (native + proxy).
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/emrul/container-init/internal/cgroup"
	"github.com/emrul/container-init/internal/pid1"
	"github.com/emrul/container-init/internal/supervisor"
	"github.com/emrul/container-init/internal/trace"
	"github.com/emrul/container-init/unit"
)

func main() {
	dir := flag.String("units", "/etc/container-init/units", "directory containing core .service / .socket files")
	dropIn := flag.String("drop-in", "/etc/container-init.d", "directory containing image-author drop-ins (override core by name)")
	strict := flag.Bool("strict-units", false, "fail fast on any parser warning (unknown directive / section)")
	validate := flag.Bool("validate", false, "load + parse units, print summary, exit without supervising (build-time sanity)")
	flag.Parse()

	log.SetFlags(0)
	log.SetPrefix("container-init: ")

	tracer := trace.New()
	defer tracer.Close()
	tracer.MemSnapshot("boot")

	units, warnings, overrides, err := unit.LoadOverlay([]string{*dir, *dropIn}, unit.Options{Lookup: unit.OSLookup, Strict: *strict})
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "container-init: warning: %s\n", w)
	}
	for _, o := range overrides {
		log.Printf("drop-in override: %s replaces %s with %s", o.Name, o.BasePath, o.OverridePath)
		tracer.Event("unit_overridden", map[string]any{
			"unit":         o.Name,
			"base_path":    o.BasePath,
			"override_path": o.OverridePath,
		})
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "container-init: load units: %v\n", err)
		os.Exit(64)
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
		// Strict mode already exits non-zero above on any warning; in
		// non-strict mode we still want a non-zero exit if any warning
		// was raised, so build pipelines catch directive drift.
		if len(warnings) > 0 {
			os.Exit(1)
		}
		os.Exit(0)
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
		os.Exit(64)
	}

	// SIGTERM / SIGINT → reverse shutdown.
	go pid1.ForwardSignals(func(sig os.Signal) {
		log.Printf("received %v, beginning reverse shutdown", sig)
		tracer.Event("signal_received", map[string]any{"sig": sig.String()})
		sup.Stop()
	}, sup.Done())

	tracer.Event("supervisor_start", nil)
	exit := sup.Run()
	close(dispatcherStop)
	<-dispatcher.Done()
	tracer.Event("boot_done", map[string]any{"exit": exit})
	os.Exit(exit)
}
