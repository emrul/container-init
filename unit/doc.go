// Package unit parses systemd unit files into the container-init subset
// type, validates them against the supported-directive rules, and
// resolves ${VAR} / ${VAR:-default} expansion on directive values.
package unit
