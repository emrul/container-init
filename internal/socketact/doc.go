// Package socketact implements socket activation in two modes:
//
//   native — the public listen fd is passed to the service via the
//            sd_listen_fds protocol (LISTEN_PID, LISTEN_FDS, fd 3).
//   proxy  — container-init binds the public listen fd itself, starts
//            the service on a private endpoint declared by ProxyTarget=,
//            and copies bytes between the accepted connection and the
//            service's private endpoint.
package socketact
