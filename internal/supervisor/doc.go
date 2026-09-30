// Package supervisor runs one goroutine per service, applies Restart=
// policy, evaluates conditions, fires OnFailure= chains, drives socket
// activation, and stops every unit in reverse dependency order on
// shutdown.
package supervisor
