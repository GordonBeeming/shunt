package caddy

import (
	"encoding/json"
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

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
	path, body, err := ServerForRoute("x", r)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/config/apps/http/servers/srv_x_frontend"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	var srv struct {
		Listen []string `json:"listen"`
		Routes []struct {
			Handle []struct {
				Handler   string `json:"handler"`
				ID        string `json:"@id"`
				Upstreams []struct {
					Dial string `json:"dial"`
				} `json:"upstreams"`
			} `json:"handle"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(body, &srv); err != nil {
		t.Fatal(err)
	}
	if len(srv.Listen) != 1 || srv.Listen[0] != ":5000" {
		t.Errorf("listen = %v, want [:5000]", srv.Listen)
	}
	h := srv.Routes[0].Handle[0]
	if h.Handler != "reverse_proxy" || h.ID != "app_x_http_frontend" {
		t.Errorf("handler=%q id=%q", h.Handler, h.ID)
	}
	if h.Upstreams[0].Dial != state.PlaceholderDial {
		t.Errorf("dial = %q, want placeholder %q", h.Upstreams[0].Dial, state.PlaceholderDial)
	}
}

func TestServerForRouteLayer4(t *testing.T) {
	r := state.Route{Key: "db", Kind: state.KindLayer4, ListenPort: 15432, CaddyID: "app_x_layer4_db"}
	path, body, err := ServerForRoute("x", r)
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
