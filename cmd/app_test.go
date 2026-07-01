package cmd

import (
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

func TestRoutesEqual(t *testing.T) {
	base := state.Route{Key: "chat", Kind: "http", ListenPort: 5215, Resource: "chat", Endpoint: "https", GuestPort: 5215, TLS: true, CaddyID: "app_MyApp_http_chat"}
	same := base
	if !routesEqual(base, same) {
		t.Error("identical routes should be equal (unchanged → left alone)")
	}
	// Each differing field must make the route count as changed (delete+put).
	diffs := map[string]state.Route{
		"port":     {Key: "chat", Kind: "http", ListenPort: 5216, Resource: "chat", Endpoint: "https", GuestPort: 5215, TLS: true, CaddyID: "app_MyApp_http_chat"},
		"tls":      {Key: "chat", Kind: "http", ListenPort: 5215, Resource: "chat", Endpoint: "https", GuestPort: 5215, TLS: false, CaddyID: "app_MyApp_http_chat"},
		"resource": {Key: "chat", Kind: "http", ListenPort: 5215, Resource: "chatv2", Endpoint: "https", GuestPort: 5215, TLS: true, CaddyID: "app_MyApp_http_chat"},
		"endpoint": {Key: "chat", Kind: "http", ListenPort: 5215, Resource: "chat", Endpoint: "http", GuestPort: 5215, TLS: true, CaddyID: "app_MyApp_http_chat"},
		"guest":    {Key: "chat", Kind: "http", ListenPort: 5215, Resource: "chat", Endpoint: "https", GuestPort: 5300, TLS: true, CaddyID: "app_MyApp_http_chat"},
		"caddyid":  {Key: "chat", Kind: "http", ListenPort: 5215, Resource: "chat", Endpoint: "https", GuestPort: 5215, TLS: true, CaddyID: "app_myapp_http_chat"},
	}
	for name, d := range diffs {
		if routesEqual(base, d) {
			t.Errorf("route differing in %s should not be equal", name)
		}
	}
}
