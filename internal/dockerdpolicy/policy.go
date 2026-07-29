// Package dockerdpolicy defines the stable guest contract used to ensure that
// Docker is healthy while registry access remains blocked at both the public
// API boundary and the daemon's network boundary.
package dockerdpolicy

const (
	// EnsureCommand synchronously starts or repairs the in-guest daemon. It
	// returns success only after Docker is healthy and the running dockerd
	// process proves that the offline policy is present in its environment.
	EnsureCommand = "/usr/local/bin/shunt-dockerd-offline"

	// ReadyMarker is written atomically after EnsureCommand proves both Docker
	// readiness and the daemon's pull-denial environment.
	ReadyMarker = "/run/shunt/dockerd-offline.ready"

	// AdmissionSocket is the only Docker API socket exposed to guest clients.
	AdmissionSocket = "/var/run/docker.sock"

	// BackendSocket is private to the admission proxy and dockerd.
	BackendSocket = "/run/shunt/dockerd/docker.sock"

	// PolicyVersion is persisted in ReadyMarker and dockerd's environment.
	PolicyVersion = "4"
)
