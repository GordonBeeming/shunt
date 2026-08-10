// Package dashboard is shunt's local web UI: one always-on page (per channel,
// on its own port) to browse every app's front-door ports with live up/down
// status, and switch / start / stop / park sidings with a click. Switch is a fast Caddy
// rebind (siding.Switch); Start runs the same build+start+bridge flow as `shunt up`
// (siding.Up); Stop stops the siding's guest; Park removes only that guest.
// Liveness is a
// host-side dial of each front-door port — no guest round-trip — so the page
// reflects what you'd actually reach.
package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
)

const (
	maxActionBodyBytes = 64 << 10
	maxProgressEntries = 8
	terminalStatusTTL  = time.Minute
	csrfTokenBytes     = 32
	csrfHeader         = "X-Shunt-CSRF"
)

var (
	credentialURLPattern = regexp.MustCompile(`https?://[^\s]+`)
	absolutePathPattern  = regexp.MustCompile(`(?:^|\s)/[^\s:]+`)
	secretPattern        = regexp.MustCompile(`(?i)(token|secret|password|key)=\S+`)
)

// Server holds the in-memory action status (start/stop are async, so their
// progress is surfaced back through /api/state) and a per-siding lock so
// double-clicks can't overlap a stop/start.
type Server struct {
	mu        sync.Mutex
	status    map[string]actionStatus // "project/siding" -> action progress/error
	busy      map[string]bool
	csrfToken string
	deps      serverDeps
}

type actionStatus struct {
	message   string
	progress  []string
	expiresAt time.Time
}

type serverDeps struct {
	loadRegistry func() (state.Registry, error)
	loadAppDir   func(string) (state.App, error)
	loadProject  func(string) (state.App, error)
	runtime      func(context.Context) container.RuntimeObservation
	guest        func(context.Context, string) container.GuestObservation
	healthOK     func(context.Context, state.App, state.Siding) bool
	switchSiding func(context.Context, *state.App, string) error
	upSiding     func(context.Context, state.App, state.Siding, bool, io.Writer) (state.Siding, error)
	stopSiding   func(context.Context, state.App, string) (siding.StopResult, error)
	parkSiding   func(context.Context, state.App, string) (state.Siding, error)
	actionLog    func(string)
}

func defaultServerDeps() serverDeps {
	return serverDeps{
		loadRegistry: state.LoadRegistry,
		loadAppDir:   state.LoadApp,
		loadProject:  loadApp,
		runtime:      container.ObserveSystem,
		guest:        container.ObserveGuest,
		healthOK:     siding.HealthOK,
		switchSiding: siding.Switch,
		upSiding:     siding.Up,
		stopSiding:   siding.Stop,
		parkSiding:   siding.Park,
		actionLog:    func(entry string) { log.Print(entry) },
	}
}

// NewServer builds a dashboard server.
func NewServer() *Server {
	return newServerWithDeps(defaultServerDeps())
}

func newServerWithDeps(deps serverDeps) *Server {
	return &Server{
		status:    map[string]actionStatus{},
		busy:      map[string]bool{},
		csrfToken: newCSRFToken(),
		deps:      deps,
	}
}

func newCSRFToken() string {
	buf := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// Handler wires the routes: the embedded UI plus the JSON API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/switch", s.handleSwitch)
	mux.HandleFunc("/api/start", s.handleStart)
	mux.HandleFunc("/api/stop", s.handleStop)
	mux.HandleFunc("/api/park", s.handlePark)
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
	Name          string   `json:"name"`
	Live          bool     `json:"live"`
	Base          bool     `json:"base"`
	Phase         string   `json:"phase"`   // persisted: worktree | data | guest | parked
	Runtime       string   `json:"runtime"` // observed: running | stopped | missing | runtime-unavailable
	RuntimeDetail string   `json:"runtimeDetail,omitempty"`
	Serving       bool     `json:"serving"`
	Status        string   `json:"status"`              // async action feedback
	Progress      []string `json:"progress,omitempty"`  // bounded materialization feedback
	Guest         string   `json:"guest"`               // Deprecated: derived compatibility field.
	IP            string   `json:"ip,omitempty"`        // guest IP (empty until activated)
	Dashboard     string   `json:"dashboard,omitempty"` // guest Aspire dashboard URL (empty if no IP)
}

type appView struct {
	Name       string       `json:"name"`
	LiveSiding string       `json:"liveSiding"`
	Removal    *removalView `json:"removal,omitempty"`
	Sidings    []sidingView `json:"sidings"`
	Routes     []routeView  `json:"routes"`
}

type removalView struct {
	ID         string `json:"id"`
	Siding     string `json:"siding"`
	Stage      string `json:"stage"`
	Generation string `json:"generation,omitempty"`
	StartedAt  string `json:"startedAt"`
	Age        string `json:"age"`
	Resume     string `json:"resume"`
}

type stateView struct {
	Channel   string    `json:"channel"`
	CSRFToken string    `json:"csrfToken"`
	Apps      []appView `json:"apps"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if !validLoopbackHost(r.Host) {
		http.Error(w, "dashboard state requires a loopback Host", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.pruneExpiredActions()
	ctx := r.Context()
	reg, err := s.deps.loadRegistry()
	if err != nil {
		httpErr(w, err)
		return
	}
	view := stateView{Channel: config.Current().Channel, CSRFToken: s.csrfToken, Apps: []appView{}}
	var runtime *container.RuntimeObservation
	names := make([]string, 0, len(reg.Projects))
	for n := range reg.Projects {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		app, err := s.deps.loadAppDir(reg.Projects[name])
		if err != nil {
			continue // a project whose state is missing/unreadable — skip, don't fail the page
		}
		live := app.LiveSiding
		if live == state.HostTarget {
			live = ""
		}
		if _, exists := app.Sidings[live]; live != "" && !exists {
			live = ""
		}
		av := appView{Name: app.Name, LiveSiding: live, Routes: []routeView{}, Sidings: []sidingView{}, Removal: removalStatus(app.Removal)}

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

		// Sidings (sorted) keep persisted materialization separate from the
		// observed Apple runtime. A failed inspect is never presented as stopped.
		snames := make([]string, 0, len(app.Sidings))
		for n := range app.Sidings {
			if n == state.HostTarget {
				continue
			}
			snames = append(snames, n)
		}
		sort.Strings(snames)
		for _, sn := range snames {
			sd := app.Sidings[sn]
			phase := effectivePhase(sd)
			observation := container.RuntimeObservation{State: container.RuntimeRunning}
			if phase == state.PhaseGuest {
				if runtime == nil {
					observed := s.deps.runtime(ctx)
					runtime = &observed
				}
				observation = *runtime
			}
			runtimeState, runtimeDetail := s.observeRuntime(ctx, sd, phase, observation)
			serving := runtimeState == "running" && s.deps.healthOK(ctx, app, sd)
			action := s.getAction(name, sn)
			sv := sidingView{
				Name:          sn,
				Live:          live == sn,
				Base:          app.BaseSiding == sn,
				Phase:         string(phase),
				Runtime:       runtimeState,
				RuntimeDetail: runtimeDetail,
				Serving:       serving,
				Status:        action.message,
				Progress:      action.progress,
				Guest:         compatibilityGuest(runtimeState, serving),
			}
			// Only surface the guest IP + dashboard link when the app is actually
			// serving. A stopped/idle siding's saved LastIP is stale and unreachable,
			// so showing it reads as "up" when it isn't (exactly the post-crash trap).
			if serving {
				sv.IP = sd.LastIP
				sv.Dashboard = siding.DashboardURL(app, sd)
			}
			av.Sidings = append(av.Sidings, sv)
		}
		view.Apps = append(view.Apps, av)
	}
	writeJSON(w, view)
}

func compatibilityGuest(runtime string, serving bool) string {
	if runtime == "running" {
		if serving {
			return "running"
		}
		return "idle"
	}
	return "stopped"
}

func effectivePhase(sd state.Siding) state.MaterializationPhase {
	if sd.MaterializationPhase == "" {
		return state.PhaseGuest
	}
	return sd.MaterializationPhase
}

func (s *Server) observeRuntime(ctx context.Context, sd state.Siding, phase state.MaterializationPhase, runtime container.RuntimeObservation) (string, string) {
	if phase != state.PhaseGuest {
		return "missing", "no guest is materialized for this phase"
	}
	if runtime.State == container.RuntimeStopped {
		detail := runtime.Detail
		if detail == "" {
			detail = "Apple container runtime is stopped"
		}
		return "stopped", detail
	}
	if runtime.State != container.RuntimeRunning {
		detail := runtime.Detail
		if detail == "" {
			detail = "Apple container runtime is unavailable"
		}
		return "runtime-unavailable", detail
	}
	switch s.deps.guest(ctx, sd.Container).State {
	case container.GuestRunning:
		return "running", ""
	case container.GuestStopped:
		return "stopped", ""
	case container.GuestAbsent:
		return "missing", "guest is not materialized"
	default:
		return "runtime-unavailable", "guest inspection unavailable"
	}
}

func removalStatus(removal *state.RemovalOperation) *removalView {
	if removal == nil {
		return nil
	}
	age := "unknown"
	if started, err := time.Parse(time.RFC3339Nano, removal.StartedAt); err == nil {
		age = time.Since(started).Round(time.Second).String()
	}
	return &removalView{ID: removal.ID, Siding: removal.Siding, Stage: string(removal.Stage), Generation: removal.GenerationID, StartedAt: removal.StartedAt, Age: age, Resume: config.Current().BinaryName + " rm " + removal.Siding}
}

// handleSwitch repoints the front door at a siding (fast Caddy rebind).
func (s *Server) handleSwitch(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	project, sd, ok := s.decodeAction(w, r)
	if !ok {
		return
	}
	// A new action supersedes any stale status for this siding immediately — so the
	// error shown always relates to the action you just took, not a previous one
	// (switch is synchronous, so "switching…" is momentary; the point is the clear).
	app, err := s.deps.loadProject(project)
	if err != nil {
		httpErr(w, err)
		return
	}
	if !requireNoRemovalAction(w, app) {
		return
	}
	if _, valid := requireGuestAction(w, app, sd, "switch"); !valid {
		return
	}
	s.setStatus(project, sd, "switching…")
	if err := s.deps.switchSiding(r.Context(), &app, sd); err != nil {
		message := "error: " + err.Error()
		s.logAction("switch", project, sd, started, r.Context(), false, message)
		s.complete(project+"/"+sd, message)
		httpErr(w, err)
		return
	}
	s.logAction("switch", project, sd, started, r.Context(), false, "")
	s.complete(project+"/"+sd, "")
	writeJSON(w, map[string]string{"ok": "switched to " + sd})
}

// handleStart brings a siding online — the same guest-liveness + build/start +
// bridge flow as `shunt up` (siding.Up), so the Start button and the CLI behave
// identically. It's slow, so it runs detached and the page polls /api/state for
// the "starting…" status; the liveness dots go green as routes come up.
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	project, sdName, ok := s.decodeAction(w, r)
	if !ok {
		return
	}
	key := project + "/" + sdName
	if !s.acquire(key) {
		http.Error(w, "an action is already running for this siding", http.StatusConflict)
		return
	}
	app, err := s.deps.loadProject(project)
	if err != nil {
		s.release(key, "")
		httpErr(w, err)
		return
	}
	if !requireNoRemovalAction(w, app) {
		s.release(key, "")
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
		started := time.Now()
		// Detached from the request; give a full build + bridge room to run while
		// the dashboard retains a bounded trail of its current progress.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		msg := ""
		if _, err := s.deps.upSiding(ctx, app, sd, true, s.progressWriter(key)); err != nil {
			msg = "error: " + err.Error()
		}
		s.logAction("start", project, sdName, started, ctx, false, msg)
		s.release(key, msg)
	}()
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"ok": "starting " + sdName})
}

// handleStop stops a siding's guest, keeping its worktree + data so Start can
// bring it back; the `stopped` marker keeps `ls`/the picker honest. It's quick but
// still async so the page stays responsive.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	project, sdName, ok := s.decodeAction(w, r)
	if !ok {
		return
	}
	key := project + "/" + sdName
	if !s.acquire(key) {
		http.Error(w, "an action is already running for this siding", http.StatusConflict)
		return
	}
	app, err := s.deps.loadProject(project)
	if err != nil {
		s.release(key, "")
		httpErr(w, err)
		return
	}
	if !requireNoRemovalAction(w, app) {
		s.release(key, "")
		return
	}
	if _, valid := requireGuestAction(w, app, sdName, "stop"); !valid {
		s.release(key, "")
		return
	}
	s.setStatus(project, sdName, "stopping…")
	go func() {
		started := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		msg := ""
		result, err := s.deps.stopSiding(ctx, app, sdName)
		if err != nil {
			msg = "error: " + err.Error()
		} else if result.Forced {
			msg = "stopped (forced)"
		}
		s.logAction("stop", project, sdName, started, ctx, result.Forced, msg)
		s.release(key, msg)
	}()
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"ok": "stopping " + sdName})
}

// handlePark removes only a non-live siding's recreatable guest. Its worktree,
// branch, data, and output remain, and Start materializes a new guest later.
func (s *Server) handlePark(w http.ResponseWriter, r *http.Request) {
	project, sdName, ok := s.decodeAction(w, r)
	if !ok {
		return
	}
	key := project + "/" + sdName
	if !s.acquire(key) {
		http.Error(w, "an action is already running for this siding", http.StatusConflict)
		return
	}
	app, err := s.deps.loadProject(project)
	if err != nil {
		s.release(key, "")
		httpErr(w, err)
		return
	}
	if !requireNoRemovalAction(w, app) {
		s.release(key, "")
		return
	}
	if app.LiveSiding == sdName {
		s.release(key, "")
		http.Error(w, fmt.Sprintf("siding %q is live; switch away before parking it", sdName), http.StatusConflict)
		return
	}
	if _, valid := requireGuestAction(w, app, sdName, "park"); !valid {
		s.release(key, "")
		return
	}
	s.setStatus(project, sdName, "parking…")
	go func() {
		started := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		msg := ""
		if _, err := s.deps.parkSiding(ctx, app, sdName); err != nil {
			msg = "error: " + err.Error()
		}
		s.logAction("park", project, sdName, started, ctx, false, msg)
		s.release(key, msg)
	}()
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"ok": "parking " + sdName})
}

func requireGuestAction(w http.ResponseWriter, app state.App, name, action string) (state.Siding, bool) {
	sd, exists := app.Sidings[name]
	if !exists || name == state.HostTarget {
		http.Error(w, "no siding "+name, http.StatusNotFound)
		return state.Siding{}, false
	}
	phase := effectivePhase(sd)
	if phase != state.PhaseGuest {
		http.Error(w, fmt.Sprintf("cannot %s siding %q while it is %s; run `shunt up %s` first", action, name, phase, name), http.StatusConflict)
		return state.Siding{}, false
	}
	return sd, true
}

func requireNoRemovalAction(w http.ResponseWriter, app state.App) bool {
	if app.Removal == nil {
		return true
	}
	removal := removalStatus(app.Removal)
	http.Error(w, fmt.Sprintf("removal %q is at stage %q; resume it with `%s`", removal.ID, removal.Stage, removal.Resume), http.StatusConflict)
	return false
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

func (s *Server) decodeAction(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return "", "", false
	}
	if !validLoopbackHost(r.Host) {
		http.Error(w, "dashboard actions require a loopback Host", http.StatusForbidden)
		return "", "", false
	}
	if !sameOrigin(r) {
		http.Error(w, "dashboard actions require the dashboard origin", http.StatusForbidden)
		return "", "", false
	}
	if s.csrfToken == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get(csrfHeader)), []byte(s.csrfToken)) != 1 {
		http.Error(w, "dashboard action token is missing or invalid", http.StatusForbidden)
		return "", "", false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "actions require Content-Type application/json", http.StatusUnsupportedMediaType)
		return "", "", false
	}
	if r.ContentLength > maxActionBodyBytes {
		http.Error(w, "action body is too large", http.StatusRequestEntityTooLarge)
		return "", "", false
	}
	var a actionReq
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxActionBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&a); err != nil || a.Project == "" || a.Siding == "" {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "action body is too large", http.StatusRequestEntityTooLarge)
			return "", "", false
		}
		http.Error(w, "need {project, siding}", http.StatusBadRequest)
		return "", "", false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "need one action object", http.StatusBadRequest)
		return "", "", false
	}
	return a.Project, a.Siding, true
}

func validLoopbackHost(host string) bool {
	name, _, err := net.SplitHostPort(host)
	if err != nil {
		name = host
	}
	name = strings.Trim(strings.TrimSpace(name), "[]")
	if strings.EqualFold(name, "localhost") {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}

func sameOrigin(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.Scheme == "" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return origin.Scheme == scheme && strings.EqualFold(origin.Host, r.Host) && validLoopbackHost(origin.Host)
}

func (s *Server) getAction(project, sd string) actionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := project + "/" + sd
	entry, ok := s.status[key]
	if !ok {
		return actionStatus{}
	}
	if !entry.expiresAt.IsZero() && !time.Now().Before(entry.expiresAt) {
		delete(s.status, key)
		return actionStatus{}
	}
	entry.progress = append([]string(nil), entry.progress...)
	return entry
}

func (s *Server) pruneExpiredActions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for key, entry := range s.status {
		if !entry.expiresAt.IsZero() && !now.Before(entry.expiresAt) {
			delete(s.status, key)
		}
	}
}

func (s *Server) setStatus(project, sd, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := project + "/" + sd
	if msg == "" {
		delete(s.status, key)
		return
	}
	entry := s.status[key]
	entry.message = msg
	entry.progress = nil
	entry.expiresAt = time.Time{}
	s.status[key] = entry
}

func (s *Server) complete(key, finalStatus string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, key)
	if finalStatus == "" {
		delete(s.status, key)
		return
	}
	entry := s.status[key]
	entry.message = finalStatus
	entry.expiresAt = time.Now().Add(terminalStatusTTL)
	s.status[key] = entry
}

type progressWriter struct {
	server *Server
	key    string
}

func (s *Server) progressWriter(key string) io.Writer {
	return progressWriter{server: s, key: key}
}

func (w progressWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		w.server.addProgress(w.key, line)
	}
	return len(p), nil
}

func (s *Server) addProgress(key, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.status[key]
	if !ok {
		return
	}
	entry.progress = append(entry.progress, line)
	if len(entry.progress) > maxProgressEntries {
		entry.progress = append([]string(nil), entry.progress[len(entry.progress)-maxProgressEntries:]...)
	}
	s.status[key] = entry
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
	s.complete(key, finalStatus)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) logAction(action, project, siding string, started time.Time, ctx context.Context, forced bool, message string) {
	state := "completed"
	if message != "" {
		state = "failed"
	}
	cancelled := ctx.Err() != nil
	s.deps.actionLog(fmt.Sprintf("dashboard_action action=%q project=%q siding=%q result=%q elapsed=%q cancelled=%t timed_out=%t forced_stop=%t error=%q", action, project, siding, state, time.Since(started).Round(time.Millisecond), cancelled, errors.Is(ctx.Err(), context.DeadlineExceeded), forced, boundedActionDetail(message)))
}

func boundedActionDetail(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	message = credentialURLPattern.ReplaceAllString(message, "<redacted-url>")
	message = absolutePathPattern.ReplaceAllStringFunc(message, func(value string) string {
		if strings.HasPrefix(value, " ") {
			return " <redacted-path>"
		}
		return "<redacted-path>"
	})
	message = secretPattern.ReplaceAllString(message, "$1=<redacted>")
	if len(message) > 256 {
		return message[:256] + "…"
	}
	return message
}

func httpErr(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
