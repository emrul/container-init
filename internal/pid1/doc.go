// Package pid1 owns PID 1 responsibilities: SIGCHLD-driven reaping,
// signal forwarding, and reverse-shutdown sequencing.
//
// The Dispatcher is the canonical wait4(-1) reaper. It owns SIGCHLD,
// drains every reapable child on each signal, and routes statuses to
// per-pid channels via Spawn / Track. Untracked pids (orphan
// grandchildren from dbus-launch / Type=forking units, anything
// reparented onto PID 1) are reaped silently. Design note
// #1 ("a separate wait4 reaper races os/exec.Cmd.Wait and steals
// exit statuses") is closed because the supervisor never calls
// cmd.Wait — Spawn is the single fork-exec entry point and the
// dispatcher is the single source of reap status.
package pid1
