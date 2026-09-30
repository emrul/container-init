// Package pid1 owns PID 1 responsibilities: SIGCHLD-driven reaping and
// signal forwarding.
//
// The Dispatcher is the process's only wait4(-1) reaper. It owns
// SIGCHLD, drains every reapable child on each signal, and routes
// statuses to per-pid channels registered by Spawn / Track. Untracked
// pids (orphaned grandchildren, anything reparented onto PID 1) are
// reaped silently. Nothing else may wait on children: Spawn is the
// single fork-exec entry point, and callers never call cmd.Wait, which
// would race the dispatcher for exit statuses.
package pid1
