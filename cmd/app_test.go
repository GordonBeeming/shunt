package cmd

import (
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

func TestRoutesEqual(t *testing.T) {
	base := state.Route{Key: "chat", Kind: "http", ListenPort: 5215, Resource: "chat", Endpoint: "https", GuestPort: 5215, TLS: true}
	same := base
	if !routesEqual(base, same) {
		t.Error("identical routes should be equal (unchanged → left alone)")
	}
	// Each differing field must make the route count as changed (delete+put).
	diffs := map[string]state.Route{
		"port":     {Key: "chat", Kind: "http", ListenPort: 5216, Resource: "chat", Endpoint: "https", GuestPort: 5215, TLS: true},
		"tls":      {Key: "chat", Kind: "http", ListenPort: 5215, Resource: "chat", Endpoint: "https", GuestPort: 5215, TLS: false},
		"resource": {Key: "chat", Kind: "http", ListenPort: 5215, Resource: "chatv2", Endpoint: "https", GuestPort: 5215, TLS: true},
		"endpoint": {Key: "chat", Kind: "http", ListenPort: 5215, Resource: "chat", Endpoint: "http", GuestPort: 5215, TLS: true},
		"guest":    {Key: "chat", Kind: "http", ListenPort: 5215, Resource: "chat", Endpoint: "https", GuestPort: 5300, TLS: true},
	}
	for name, d := range diffs {
		if routesEqual(base, d) {
			t.Errorf("route differing in %s should not be equal", name)
		}
	}
}
