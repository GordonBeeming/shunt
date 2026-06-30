package caddy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/state"
)

// DevCertPath / DevCertKeyPath are where shunt exports the host's dotnet HTTPS
// dev certificate (PEM) for Caddy to serve — the cert the host already trusts,
// so no new root CA is introduced.
func DevCertPath() (string, error)    { return devCertFile("devcert.pem") }
func DevCertKeyPath() (string, error) { return devCertFile("devcert.key") }

func devCertFile(name string) (string, error) {
	dir, err := config.GlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "caddy", name), nil
}

// TLSAppBody is the Caddy `tls` app config that serves the host dotnet dev cert
// (used by both the init bootstrap and `cert install` against a running Caddy).
func TLSAppBody() ([]byte, error) {
	cert, err := DevCertPath()
	if err != nil {
		return nil, err
	}
	key, err := DevCertKeyPath()
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"certificates": map[string]any{
			"load_files": []any{map[string]any{"certificate": cert, "key": key}},
		},
	})
}

// ExportDevCert writes the host's dotnet dev cert to PEM for Caddy. `dotnet
// dev-certs https` creates the cert if it doesn't exist; the host should have
// trusted it once with `dotnet dev-certs https --trust`.
func ExportDevCert(ctx context.Context) error {
	cert, err := DevCertPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cert), 0o755); err != nil {
		return err
	}
	if _, err := proc.Run(ctx, "dotnet", "dev-certs", "https",
		"--export-path", cert, "--format", "PEM", "--no-password"); err != nil {
		return fmt.Errorf("export dotnet dev cert (run `dotnet dev-certs https --trust` once): %w", err)
	}
	return nil
}

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
	certPath, err := DevCertPath()
	if err != nil {
		return nil, err
	}
	keyPath, err := DevCertKeyPath()
	if err != nil {
		return nil, err
	}
	doc := map[string]any{
		"admin": map[string]any{"listen": config.AdminAddr()},
		"apps": map[string]any{
			"http":   map[string]any{"servers": map[string]any{}},
			"layer4": map[string]any{"servers": map[string]any{}},
			// Serve HTTPS with the host's dotnet dev certificate (the one `dotnet
			// dev-certs https --trust` already trusts) — NOT a Caddy-rolled internal
			// CA. `shunt init` exports it to these PEM files; no new root cert is
			// added to the trust store.
			"tls": map[string]any{
				"certificates": map[string]any{
					"load_files": []any{map[string]any{
						"certificate": certPath,
						"key":         keyPath,
					}},
				},
			},
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
		if r.TLS {
			// Serve HTTPS at the edge (host match makes automatic HTTPS provision
			// the localhost cert).
			server["tls_connection_policies"] = []any{map[string]any{}}
			server["routes"] = []any{map[string]any{
				"match":  []any{map[string]any{"host": []string{"localhost", "127.0.0.1"}}},
				"handle": []any{handler},
			}}
			// Dial the upstream over TLS only when it actually serves https
			// (endpoint != "http"), skipping verification of its self-signed dev
			// cert. An http upstream (e.g. the Aspire dashboard) is dialed plain
			// http, so a TLS front door can still front a plain-http app.
			if r.Endpoint != "http" {
				handler["transport"] = map[string]any{
					"protocol": "http",
					"tls":      map[string]any{"insecure_skip_verify": true},
				}
			}
		}
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
