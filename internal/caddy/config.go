package caddy

import (
	"encoding/json"
	"fmt"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/state"
)

// RouteID is the @id shunt stamps on a route's proxy handler so switch can PATCH
// its upstream directly: app_<app>_<kind>_<key>.
func RouteID(app, kind, key string) string {
	return fmt.Sprintf("app_%s_%s_%s", app, kind, key)
}

// ServerName is the per-route Caddy server name (one stable listen port each).
func ServerName(app, key string) string {
	return fmt.Sprintf("srv_%s_%s", app, key)
}

// Bootstrap is the minimal config shunt loads into a fresh Caddy: the admin
// endpoint plus empty http and layer4 apps that `app add` later fills with one
// server per stable front-door port.
func Bootstrap() ([]byte, error) {
	doc := map[string]any{
		"admin": map[string]any{"listen": config.AdminAddr()},
		"apps": map[string]any{
			"http":   map[string]any{"servers": map[string]any{}},
			"layer4": map[string]any{"servers": map[string]any{}},
		},
	}
	return json.MarshalIndent(doc, "", "  ")
}

// ServerForRoute builds the Caddy server object for one front-door route: it
// listens on the stable port and proxies to a placeholder dial until a siding
// goes live. The handler carries the route's @id for live PATCHing.
//
// Returns the admin config path to PUT it at and the JSON body.
func ServerForRoute(app string, r state.Route) (path string, body []byte, err error) {
	handler := map[string]any{"@id": r.CaddyID}
	server := map[string]any{
		"listen": []string{fmt.Sprintf(":%d", r.ListenPort)},
		"routes": []any{map[string]any{"handle": []any{handler}}},
	}
	switch r.Kind {
	case state.KindHTTP:
		handler["handler"] = "reverse_proxy"
		handler["upstreams"] = []any{map[string]any{"dial": state.PlaceholderDial}}
		body, err = json.Marshal(server)
		return fmt.Sprintf("/config/apps/http/servers/%s", ServerName(app, r.Key)), body, err
	case state.KindLayer4:
		handler["handler"] = "proxy"
		handler["upstreams"] = []any{map[string]any{"dial": []string{state.PlaceholderDial}}}
		body, err = json.Marshal(server)
		return fmt.Sprintf("/config/apps/layer4/servers/%s", ServerName(app, r.Key)), body, err
	default:
		return "", nil, fmt.Errorf("unknown route kind %q", r.Kind)
	}
}

// DialPatch returns the admin path + body to repoint a route's upstream at
// host:port, matched to the route kind (http vs layer4 dial shapes differ).
func DialPatch(r state.Route, hostPort string) (path string, body []byte, err error) {
	switch r.Kind {
	case state.KindHTTP:
		body, err = json.Marshal(map[string]any{"dial": hostPort})
		return "/id/" + r.CaddyID + "/upstreams/0", body, err
	case state.KindLayer4:
		body, err = json.Marshal([]string{hostPort})
		return "/id/" + r.CaddyID + "/upstreams/0/dial", body, err
	default:
		return "", nil, fmt.Errorf("unknown route kind %q", r.Kind)
	}
}

// routeApp recovers the app name from a route's CaddyID (app_<app>_<kind>_<key>).
// The CaddyID is authoritative, so server names stay consistent with it.
func routeApp(r state.Route) string {
	// CaddyID format: app_<app>_<kind>_<key>. Split off the fixed prefix/suffix.
	// We only need <app>; keys/kinds never contain underscores by construction.
	const prefix = "app_"
	s := r.CaddyID
	if len(s) > len(prefix) {
		s = s[len(prefix):]
	}
	// trim trailing _<kind>_<key>
	for i := 0; i < 2; i++ {
		if idx := lastIndexByte(s, '_'); idx >= 0 {
			s = s[:idx]
		}
	}
	return s
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}
