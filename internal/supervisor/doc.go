// Package supervisor runs one goroutine per service, applies Restart=
// policy, evaluates conditions, fires OnFailure= chains, and signals the
// pid1 package to drive reverse shutdown when ExitContainerOnFailure=true.
package supervisor
