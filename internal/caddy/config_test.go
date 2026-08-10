package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

func TestSnapshotAndRestoreRoutesTouchesOnlyAffectedServers(t *testing.T) {
	oldBody := json.RawMessage(`{"listen":["127.0.0.1:5000"],"routes":[{"old":true}]}`)
	configBody := `{"apps":{"http":{"servers":{"srv_sample_web":` + string(oldBody) + `,"srv_other_web":{"unrelated":true}}},"layer4":{"servers":{}}}}`
	var deleted []string
	var operations []string
	putBodies := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/config/":
			_, _ = io.WriteString(w, configBody)
		case request.Method == http.MethodDelete:
			operations = append(operations, "delete "+request.URL.Path)
			deleted = append(deleted, request.URL.Path)
			if request.URL.Path == "/config/apps/layer4/servers/srv_sample_db" {
				http.NotFound(w, request)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodPut:
			operations = append(operations, "put "+request.URL.Path)
			body, _ := io.ReadAll(request.Body)
			putBodies[request.URL.Path] = body
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)
	admin := &Admin{base: server.URL, http: server.Client()}
	routes := []state.Route{
		{Key: "web", Kind: state.KindHTTP},
		{Key: "db", Kind: state.KindLayer4},
	}
	snapshot, err := SnapshotRoutes(context.Background(), admin, "sample", routes)
	if err != nil {
		t.Fatal(err)
	}
	if err := RestoreRoutes(context.Background(), admin, snapshot); err != nil {
		t.Fatal(err)
	}
	wantHTTP := "/config/apps/http/servers/srv_sample_web"
	wantLayer4 := "/config/apps/layer4/servers/srv_sample_db"
	if len(deleted) != 2 || deleted[0] != wantHTTP || deleted[1] != wantLayer4 {
		t.Fatalf("deleted=%v", deleted)
	}
	if !bytes.Equal(putBodies[wantHTTP], oldBody) || putBodies[wantLayer4] != nil {
		t.Fatalf("put bodies=%v", putBodies)
	}
	if _, touched := putBodies["/config/apps/http/servers/srv_other_web"]; touched {
		t.Fatal("unrelated route was overwritten")
	}
	if len(operations) != 3 || operations[0] != "delete "+wantHTTP || operations[1] != "delete "+wantLayer4 || operations[2] != "put "+wantHTTP {
		t.Fatalf("restore ordering=%v", operations)
	}
}

func TestRouteIDAndServerName(t *testing.T) {
	if got, want := RouteID("myapp", "http", "frontend"), "app_myapp_http_frontend"; got != want {
		t.Errorf("RouteID = %q, want %q", got, want)
	}
	if got, want := ServerName("myapp", "db"), "srv_myapp_db"; got != want {
		t.Errorf("ServerName = %q, want %q", got, want)
	}
}

func TestServerForRouteHTTP(t *testing.T) {
	r := state.Route{Key: "frontend", Kind: state.KindHTTP, ListenPort: 5000, CaddyID: "app_x_http_frontend"}
	path, body, err := ServerForRoute("x", r, true) // disableCache: true
	if err != nil {
		t.Fatal(err)
	}
	if want := "/config/apps/http/servers/srv_x_frontend"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	var srv struct {
		Listen    []string `json:"listen"`
		Protocols []string `json:"protocols"`
		Routes    []struct {
			Handle []struct {
				Handler   string `json:"handler"`
				ID        string `json:"@id"`
				Upstreams []struct {
					Dial string `json:"dial"`
				} `json:"upstreams"`
				Headers struct {
					Response struct {
						Set map[string][]string `json:"set"`
					} `json:"response"`
				} `json:"headers"`
			} `json:"handle"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(body, &srv); err != nil {
		t.Fatal(err)
	}
	if len(srv.Listen) != 1 || srv.Listen[0] != "127.0.0.1:5000" {
		t.Errorf("listen = %v, want [127.0.0.1:5000]", srv.Listen)
	}
	// HTTP/3 disabled — protocols must be h1/h2 only (no h3), so no QUIC Alt-Svc.
	if len(srv.Protocols) != 2 || srv.Protocols[0] != "h1" || srv.Protocols[1] != "h2" {
		t.Errorf("protocols = %v, want [h1 h2] (no h3)", srv.Protocols)
	}
	h := srv.Routes[0].Handle[0]
	if h.Handler != "reverse_proxy" || h.ID != "app_x_http_frontend" {
		t.Errorf("handler=%q id=%q", h.Handler, h.ID)
	}
	if h.Upstreams[0].Dial != state.PlaceholderDial {
		t.Errorf("dial = %q, want placeholder %q", h.Upstreams[0].Dial, state.PlaceholderDial)
	}
	// disableCache: true -> Cache-Control: no-store on responses.
	if got := h.Headers.Response.Set["Cache-Control"]; len(got) != 1 || got[0] != "no-store" {
		t.Errorf("Cache-Control = %v, want [no-store]", got)
	}
}

func TestServerForRouteHTTPNoDisableCache(t *testing.T) {
	r := state.Route{Key: "frontend", Kind: state.KindHTTP, ListenPort: 5000, CaddyID: "app_x_http_frontend"}
	_, body, err := ServerForRoute("x", r, false) // disableCache: false
	if err != nil {
		t.Fatal(err)
	}
	// No `headers` block when caching isn't disabled.
	if bytes.Contains(body, []byte("Cache-Control")) {
		t.Errorf("did not expect Cache-Control header when disableCache=false: %s", body)
	}
}

func TestServerForRouteLayer4(t *testing.T) {
	r := state.Route{Key: "db", Kind: state.KindLayer4, ListenPort: 15432, CaddyID: "app_x_layer4_db"}
	path, body, err := ServerForRoute("x", r, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/config/apps/layer4/servers/srv_x_db"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	// layer4 proxy dial is an array of strings.
	var srv struct {
		Routes []struct {
			Handle []struct {
				Handler   string `json:"handler"`
				Upstreams []struct {
					Dial []string `json:"dial"`
				} `json:"upstreams"`
			} `json:"handle"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(body, &srv); err != nil {
		t.Fatal(err)
	}
	h := srv.Routes[0].Handle[0]
	if h.Handler != "proxy" {
		t.Errorf("handler = %q, want proxy", h.Handler)
	}
	if len(h.Upstreams[0].Dial) != 1 || h.Upstreams[0].Dial[0] != state.PlaceholderDial {
		t.Errorf("dial = %v, want [%s]", h.Upstreams[0].Dial, state.PlaceholderDial)
	}
}

func TestDialPatch(t *testing.T) {
	httpRoute := state.Route{Kind: state.KindHTTP, CaddyID: "app_x_http_frontend"}
	path, body, err := DialPatch(httpRoute, "1.2.3.4:80")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/id/app_x_http_frontend/upstreams/0"; path != want {
		t.Errorf("http path = %q, want %q", path, want)
	}
	if got, want := string(body), `{"dial":"1.2.3.4:80"}`; got != want {
		t.Errorf("http body = %s, want %s", got, want)
	}

	l4Route := state.Route{Kind: state.KindLayer4, CaddyID: "app_x_layer4_db"}
	path, body, err = DialPatch(l4Route, "1.2.3.4:5432")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/id/app_x_layer4_db/upstreams/0/dial"; path != want {
		t.Errorf("l4 path = %q, want %q", path, want)
	}
	if got, want := string(body), `["1.2.3.4:5432"]`; got != want {
		t.Errorf("l4 body = %s, want %s", got, want)
	}
}

func TestParseDial(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"http string dial", `{"handler":"reverse_proxy","upstreams":[{"dial":"1.2.3.4:80"}]}`, "1.2.3.4:80", false},
		{"layer4 array dial", `{"handler":"proxy","upstreams":[{"dial":["1.2.3.4:5432"]}]}`, "1.2.3.4:5432", false},
		{"no upstreams", `{"@id":"app_x_http_y"}`, "", true},
		{"empty upstreams", `{"upstreams":[]}`, "", true},
		{"garbage", `not json`, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseDial([]byte(c.raw))
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("parseDial = %q, want %q", got, c.want)
			}
		})
	}
}

func TestBootstrapHasApps(t *testing.T) {
	body, err := Bootstrap()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Admin struct {
			Listen string `json:"listen"`
		} `json:"admin"`
		Apps struct {
			HTTP   map[string]any `json:"http"`
			Layer4 map[string]any `json:"layer4"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Admin.Listen == "" {
		t.Error("bootstrap admin listen is empty")
	}
	if doc.Apps.HTTP == nil || doc.Apps.Layer4 == nil {
		t.Error("bootstrap must define both http and layer4 apps")
	}
}
