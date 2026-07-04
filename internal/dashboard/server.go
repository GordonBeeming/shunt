// Package dashboard is shunt's local web UI: one always-on page (per channel,
// on its own port) to browse every app's front-door ports with live up/down
// status, and switch / start / stop sidings with a click. Switch is a fast Caddy
// rebind (siding.Switch); Start runs the same build+start+bridge flow as `shunt up`
// (siding.Up); Stop stops the siding's guest (container.Stop). Liveness is a
// host-side dial of each front-door port — no guest round-trip — so the page
// reflects what you'd actually reach.
package dashboard

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
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

// Server holds the in-memory action status (start/stop are async, so their
// progress is surfaced back through /api/state) and a per-siding lock so
// double-clicks can't overlap a stop/start.
type Server struct {
	mu     sync.Mutex
	status map[string]string // "project/siding" -> "switching"|"starting…"|"stopping…"|"error: …"|""
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
	mux.HandleFunc("/api/start", s.handleStart)
	mux.HandleFunc("/api/stop", s.handleStop)
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

		// The host (your local copy) is a switch target too — list it first. Its
		// "running" state can't be probed like a guest, so infer it from the front
		// door: the host can only serve once it's the live target (Caddy steps aside),
		// so when it's live we read its routes — any up means the native app is
		// running, none up means it's live but idle (start it). Not live → "local",
		// which the UI treats as "switch to it first" (no start/stop).
		hostGuest := "local"
		if app.LiveSiding == state.HostTarget {
			hostGuest = "idle"
			for _, rt := range av.Routes {
				if rt.Up {
					hostGuest = "running"
					break
				}
			}
		}
		av.Sidings = append(av.Sidings, sidingView{
			Name:   state.HostTarget,
			Live:   app.LiveSiding == state.HostTarget,
			Guest:  hostGuest,
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
			// Map the raw container state to a display status:
			//   running + HealthOK -> "running"  (the app's health endpoint answers)
			//   running + !HealthOK -> "idle"     (guest up, app not serving yet)
			//   "" (guest gone, or the container runtime is down after a crash/reboot)
			//     -> "stopped", so the row still offers Start instead of going blank
			//     with a stale IP.
			guest := st
			switch st {
			case "running":
				if siding.HealthOK(ctx, app, sd) {
					guest = "running"
				} else {
					guest = "idle"
				}
			case "":
				guest = "stopped"
			}
			sv := sidingView{
				Name:   sn,
				Live:   app.LiveSiding == sn,
				Guest:  guest,
				Status: s.getStatus(name, sn),
			}
			// Only surface the guest IP + dashboard link when the app is actually
			// serving. A stopped/idle siding's saved LastIP is stale and unreachable,
			// so showing it reads as "up" when it isn't (exactly the post-crash trap).
			if guest == "running" {
				sv.IP = sd.LastIP
				sv.Dashboard = siding.DashboardURL(app, sd)
			}
			av.Sidings = append(av.Sidings, sv)
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
	// A new action supersedes any stale status for this siding immediately — so the
	// error shown always relates to the action you just took, not a previous one
	// (switch is synchronous, so "switching…" is momentary; the point is the clear).
	s.setStatus(project, sd, "switching…")
	app, err := loadApp(project)
	if err != nil {
		s.setStatus(project, sd, "error: "+err.Error())
		httpErr(w, err)
		return
	}
	if err := siding.Switch(r.Context(), &app, sd); err != nil {
		s.setStatus(project, sd, "error: "+err.Error())
		httpErr(w, err)
		return
	}
	s.setStatus(project, sd, "")
	writeJSON(w, map[string]string{"ok": "switched to " + sd})
}

// handleStart brings a siding online — the same guest-liveness + build/start +
// bridge flow as `shunt up` (siding.Up), so the Start button and the CLI behave
// identically. It's slow, so it runs detached and the page polls /api/state for
// the "starting…" status; the liveness dots go green as routes come up. Host →
// HostStart (launch the native app).
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
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
	if sdName == state.HostTarget {
		s.setStatus(project, sdName, "starting…")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			msg := ""
			if err := siding.HostStart(ctx, app); err != nil {
				msg = "error: " + err.Error()
			}
			s.release(key, msg)
		}()
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]string{"ok": "starting host"})
		return
	}
	sd, ok := app.Sidings[sdName]
	if !ok {
		s.release(key, "")
		http.Error(w, "no siding "+sdName, http.StatusNotFound)
		return
	}
	s.setStatus(project, sdName, "starting…")
	go func() {
		// Detached from the request; give a full build + bridge room to run. The
		// dashboard polls status, so progress lines are discarded.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		msg := ""
		if updated, err := siding.Up(ctx, app, sd, true, io.Discard); err != nil {
			msg = "error: " + err.Error()
		} else {
			app.Sidings[sdName] = updated
			if e := state.SaveApp(app); e != nil {
				msg = "error: " + e.Error()
			}
		}
		s.release(key, msg)
	}()
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"ok": "starting " + sdName})
}

// handleStop stops a siding's guest, keeping its worktree + data so Start can
// bring it back; the `stopped` marker keeps `ls`/the picker honest. It's quick but
// still async so the page stays responsive. Host → HostStop (kill the native app).
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
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
	if sdName == state.HostTarget {
		s.setStatus(project, sdName, "stopping…")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			siding.HostStop(ctx, app)
			s.release(key, "")
		}()
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]string{"ok": "stopping host"})
		return
	}
	sd, ok := app.Sidings[sdName]
	if !ok {
		s.release(key, "")
		http.Error(w, "no siding "+sdName, http.StatusNotFound)
		return
	}
	s.setStatus(project, sdName, "stopping…")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		msg := ""
		// StopOrForce gracefully stops; only the cgroup.kill wedge (errno 95) triggers the
		// force-remove escape. A transient failure comes back as an error so the user can
		// retry rather than lose the guest; `up` self-heals a force-removed guest.
		if forced, err := container.StopOrForce(ctx, sd.Container); err != nil {
			msg = "error: " + err.Error()
		} else {
			msg = s.markStopped(project, sdName, forced) // forced: guest gone, bridges stale
		}
		s.release(key, msg)
	}()
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"ok": "stopping " + sdName})
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

// markStopped reloads the app fresh (so a concurrent action's state isn't clobbered
// by a stale in-memory snapshot from the start of this long-running stop), marks the
// siding stopped, and — when the guest was force-removed — clears its now-stale
// bridges, then persists. Returns the final status message ("" on a clean stop,
// "stopped (forced)" after a force-remove, or an "error: …" string).
func (s *Server) markStopped(project, sdName string, forced bool) string {
	app, err := loadApp(project)
	if err != nil {
		return "error: " + err.Error()
	}
	sd, ok := app.Sidings[sdName]
	if !ok {
		return ""
	}
	sd.Stopped = true
	if forced {
		sd.Bridges = nil
	}
	app.Sidings[sdName] = sd
	if err := state.SaveApp(app); err != nil {
		return "error: " + err.Error()
	}
	if forced {
		return "stopped (forced)"
	}
	return ""
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
