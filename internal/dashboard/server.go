// Package dashboard is shunt's local web UI: one always-on page (per channel,
// on its own port) to browse every app's front-door ports with live up/down
// status, and switch/restart sidings with a click. Switch is a fast Caddy rebind
// (siding.Switch); restart runs the configured stop+start in the guest
// (siding.Restart) to bring a down route up. Liveness is a host-side TCP dial of
// each front-door port — no guest round-trip — so the page reflects what you'd
// actually reach.
package dashboard

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
)

// Server holds the in-memory action status (restart is async, so its progress is
// surfaced back through /api/state) and a per-siding lock so double-clicks can't
// overlap a stop/start.
type Server struct {
	mu     sync.Mutex
	status map[string]string // "project/siding" -> "switching"|"restarting"|"error: …"|""
	busy   map[string]bool
}

// NewServer builds a dashboard server.
func NewServer() *Server {
	return &Server{status: map[string]string{}, busy: map[string]bool{}}
}

// Handler wires the routes: the embedded UI plus the JSON API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/switch", s.handleSwitch)
	mux.HandleFunc("/api/restart", s.handleRestart)
	mux.HandleFunc("/", handleIndex)
	return mux
}

// --- JSON shapes ---

type routeView struct {
	Key        string `json:"key"`
	Kind       string `json:"kind"`
	ListenPort int    `json:"listenPort"`
	URL        string `json:"url"`
	Up         bool   `json:"up"`
}

type sidingView struct {
	Name      string `json:"name"`
	Live      bool   `json:"live"`
	Guest     string `json:"guest"`               // running | stopped | …
	Status    string `json:"status"`              // async action feedback
	IP        string `json:"ip,omitempty"`        // guest IP (empty until activated)
	Dashboard string `json:"dashboard,omitempty"` // guest Aspire dashboard URL (empty if no IP)
}

type appView struct {
	Name       string       `json:"name"`
	LiveSiding string       `json:"liveSiding"`
	Sidings    []sidingView `json:"sidings"`
	Routes     []routeView  `json:"routes"`
}

type stateView struct {
	Channel string    `json:"channel"`
	Apps    []appView `json:"apps"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reg, err := state.LoadRegistry()
	if err != nil {
		httpErr(w, err)
		return
	}
	view := stateView{Channel: config.Current().Channel}
	names := make([]string, 0, len(reg.Projects))
	for n := range reg.Projects {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		app, err := state.LoadApp(reg.Projects[name])
		if err != nil {
			continue // a project whose state is missing/unreadable — skip, don't fail the page
		}
		av := appView{Name: app.Name, LiveSiding: app.LiveSiding, Routes: []routeView{}, Sidings: []sidingView{}}

		// Liveness is a front-door dial (through Caddy) — see routeUp for why we
		// can't dial the guest directly from the launchd dashboard. The guest's own
		// IP + dashboard link live on the siding row below, not per-route.
		for _, rt := range app.FrontDoor {
			av.Routes = append(av.Routes, routeView{
				Key:        rt.Key,
				Kind:       rt.Kind,
				ListenPort: rt.ListenPort,
				URL:        routeURL(rt),
				Up:         routeUp(rt),
			})
		}

		// The host (your local copy) is a switch target too — list it first.
		av.Sidings = append(av.Sidings, sidingView{
			Name:   state.HostTarget,
			Live:   app.LiveSiding == state.HostTarget,
			Guest:  "local",
			Status: s.getStatus(name, state.HostTarget),
		})

		// Sidings (sorted) with guest state + any in-flight action status.
		snames := make([]string, 0, len(app.Sidings))
		for n := range app.Sidings {
			snames = append(snames, n)
		}
		sort.Strings(snames)
		for _, sn := range snames {
			sd := app.Sidings[sn]
			st, _ := container.State(ctx, sd.Container)
			av.Sidings = append(av.Sidings, sidingView{
				Name:      sn,
				Live:      app.LiveSiding == sn,
				Guest:     st,
				Status:    s.getStatus(name, sn),
				IP:        sd.LastIP,
				Dashboard: siding.DashboardURL(app, sd), // "" when no IP; front-end enables it only when running
			})
		}
		view.Apps = append(view.Apps, av)
	}
	writeJSON(w, view)
}

// handleSwitch repoints the front door at a siding (fast Caddy rebind).
func (s *Server) handleSwitch(w http.ResponseWriter, r *http.Request) {
	project, sd, ok := decodeAction(w, r)
	if !ok {
		return
	}
	app, err := loadApp(project)
	if err != nil {
		httpErr(w, err)
		return
	}
	if err := siding.Switch(r.Context(), &app, sd); err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"ok": "switched to " + sd})
}

// handleRestart runs stop+start in the guest. It's slow, so it runs in the
// background and the page polls /api/state for the "restarting" status; liveness
// dots go green when the route comes back up.
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	project, sdName, ok := decodeAction(w, r)
	if !ok {
		return
	}
	key := project + "/" + sdName
	if !s.acquire(key) {
		http.Error(w, "an action is already running for this siding", http.StatusConflict)
		return
	}
	app, err := loadApp(project)
	if err != nil {
		s.release(key, "")
		httpErr(w, err)
		return
	}
	// Restarting the host target bounces the native app (stop+start), not a guest.
	if sdName == state.HostTarget {
		s.setStatus(project, sdName, "restarting…")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			siding.HostStop(ctx, app)
			msg := ""
			if err := siding.HostStart(ctx, app); err != nil {
				msg = "error: " + err.Error()
			}
			s.release(key, msg)
		}()
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]string{"ok": "restarting host"})
		return
	}
	sd, ok := app.Sidings[sdName]
	if !ok {
		s.release(key, "")
		http.Error(w, "no siding "+sdName, http.StatusNotFound)
		return
	}
	s.setStatus(project, sdName, "restarting…")
	go func() {
		// Detached from the request; give a full rebuild + recovery room to run.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		err := siding.Restart(ctx, app, sd)
		msg := ""
		if err != nil {
			msg = "error: " + err.Error()
		}
		s.release(key, msg)
	}()
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"ok": "restarting " + sdName})
}

// --- helpers ---

// routeUp reports whether a front-door route actually serves right now, dialing
// the stable localhost port through Caddy. We deliberately go through the front
// door rather than the guest IP: the dashboard runs as a launchd agent, and
// macOS Local Network privacy blocks a launchd process from reaching the guest's
// 192.168.64.x address (loopback is fine). Caddy's listener is always up, so a
// raw TCP dial is a false positive — for HTTP we make a real request and treat
// 502/503/504 (the front door couldn't reach the app — it's down, or the bridge/
// routing is off) or no response as down; for
// layer4 we fall back to a TCP dial. For app-vs-Caddy disambiguation, use the
// guest-direct link (opened in a real browser, which has Local Network access).
func routeUp(r state.Route) bool {
	addr := fmt.Sprintf("127.0.0.1:%d", r.ListenPort)
	if r.Kind == state.KindHTTP {
		scheme := "http"
		if r.TLS {
			scheme = "https"
		}
		client := &http.Client{
			Timeout: 1500 * time.Millisecond,
			Transport: &http.Transport{
				TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
				DisableKeepAlives: true,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		resp, err := client.Get(scheme + "://" + addr + "/")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode != http.StatusBadGateway &&
			resp.StatusCode != http.StatusServiceUnavailable &&
			resp.StatusCode != http.StatusGatewayTimeout
	}
	conn, err := net.DialTimeout("tcp", addr, 400*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func routeURL(r state.Route) string {
	if r.Kind == state.KindHTTP {
		scheme := "http"
		if r.TLS {
			scheme = "https"
		}
		return fmt.Sprintf("%s://localhost:%d", scheme, r.ListenPort)
	}
	return fmt.Sprintf("localhost:%d", r.ListenPort)
}

func loadApp(project string) (state.App, error) {
	reg, err := state.LoadRegistry()
	if err != nil {
		return state.App{}, err
	}
	dir, ok := reg.Projects[project]
	if !ok {
		return state.App{}, fmt.Errorf("no app %q", project)
	}
	return state.LoadApp(dir)
}

type actionReq struct {
	Project string `json:"project"`
	Siding  string `json:"siding"`
}

func decodeAction(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return "", "", false
	}
	var a actionReq
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil || a.Project == "" || a.Siding == "" {
		http.Error(w, "need {project, siding}", http.StatusBadRequest)
		return "", "", false
	}
	return a.Project, a.Siding, true
}

func (s *Server) getStatus(project, sd string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status[project+"/"+sd]
}

func (s *Server) setStatus(project, sd, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[project+"/"+sd] = msg
}

// acquire marks a siding busy; returns false if an action is already running.
func (s *Server) acquire(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[key] {
		return false
	}
	s.busy[key] = true
	return true
}

// release clears the busy flag and sets a final status (empty = done/clean).
func (s *Server) release(key, finalStatus string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy[key] = false
	s.status[key] = finalStatus
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
