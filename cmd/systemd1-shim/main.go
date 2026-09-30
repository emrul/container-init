// systemd1-shim runs the internal/systemd1shim package as a
// standalone process. The org.freedesktop.systemd1 surface must be
// registered on the session bus of a non-root user (e.g. kasm-user),
// which PID 1 cannot do directly: a session dbus-daemon rejects
// EXTERNAL-auth connections whose SO_PEERCRED uid is not the bus
// owner's.
//
// Run it from a container-init drop-in unit with:
//
//	User=kasm-user
//	EnvironmentFile=/tmp/kasm-dbus.env
//	ExecStart=/usr/local/bin/systemd1-shim
//
// With no flags the shim reads $DBUS_SESSION_BUS_ADDRESS from its
// environment. Pass --address to override; pass --address=system to
// register on the system bus.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/emrul/container-init/internal/systemd1shim"
)

var version = "dev"

func main() {
	addr := flag.String("address", "", "D-Bus address to register org.freedesktop.systemd1 on; defaults to $DBUS_SESSION_BUS_ADDRESS")
	versionFlag := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Println("systemd1-shim", version)
		os.Exit(0)
	}

	log.SetFlags(0)
	log.SetPrefix("systemd1-shim: ")

	target := *addr
	if target == "" {
		target = os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	}
	if target == "" {
		log.Fatalf("no --address and DBUS_SESSION_BUS_ADDRESS unset")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// SIGTERM from container-init's reverse-shutdown -> cancel context
	// -> shim closes the bus connection and exits.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("received %v, shutting down", sig)
		cancel()
	}()

	shim, err := systemd1shim.Open(ctx, []string{target}, func(format string, args ...any) {
		log.Printf(format, args...)
	})
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer shim.Close()

	<-ctx.Done()
}
