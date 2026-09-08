package hostreach

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeGuest stands in for the in-guest relay: it accepts the host's connections,
// checks the token exactly as the relay does, and lets a test drive one request.
type fakeGuest struct {
	t        *testing.T
	listener net.Listener
	token    string
	accepted chan net.Conn
	refused  chan string
}

func newFakeGuest(t *testing.T, token string) *fakeGuest {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGuest{
		t: t, listener: l, token: token,
		accepted: make(chan net.Conn, 8),
		refused:  make(chan string, 8),
	}
	t.Cleanup(func() { _ = l.Close() })
	go g.run()
	return g
}

func (g *fakeGuest) run() {
	for {
		conn, err := g.listener.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			line, err := bufio.NewReader(io.LimitReader(conn, 512)).ReadString('\n')
			if err != nil {
				_ = conn.Close()
				return
			}
			name, given, found := strings.Cut(strings.TrimSpace(line), " ")
			if !found || name != "SHUNT1" || given != g.token {
				g.refused <- given
				_ = conn.Close()
				return
			}
			if _, err := conn.Write([]byte("OK\n")); err != nil {
				_ = conn.Close()
				return
			}
			g.accepted <- conn
		}(conn)
	}
}

func (g *fakeGuest) address() string {
	_, port, _ := net.SplitHostPort(g.listener.Addr().String())
	return port
}

// serveAgainst runs one host-end connection against the fake guest, using the
// guest's own port rather than the real control port so the test needs no
// privileges and no guest.
func serveAgainst(t *testing.T, g *fakeGuest, cfg HostConfig) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	target := net.JoinHostPort("127.0.0.1", g.address())
	go func() {
		logger := log.New(io.Discard, "", 0)
		_ = serveOneForTest(ctx, target, cfg, logger)
	}()
}

// serveOneForTest is serveOne with a logger, kept here so the production path
// stays free of a parameter only a test needs.
func serveOneForTest(ctx context.Context, target string, cfg HostConfig, _ *log.Logger) error {
	return serveOne(ctx, target, cfg)
}

func TestHostRefusesToTalkToAGuestWithTheWrongToken(t *testing.T) {
	// The token is the whole reason the guest's listener is safe to have: every
	// guest on the bridge can reach it.
	g := newFakeGuest(t, "the-right-token")
	cfg := HostConfig{GuestAddress: "127.0.0.1", Token: "the-wrong-token", ProbeName: ProbeName}
	target := net.JoinHostPort("127.0.0.1", g.address())

	err := serveOne(context.Background(), target, cfg)
	if err == nil {
		t.Fatal("serveOne() = nil error, want the guest's refusal surfaced")
	}
	select {
	case given := <-g.refused:
		if given != "the-wrong-token" {
			t.Errorf("guest saw token %q", given)
		}
	case <-time.After(2 * time.Second):
		t.Error("the guest never saw a greeting")
	}
}

func TestHostRefusesAnEndpointTheContractNeverDeclared(t *testing.T) {
	// The guest names an endpoint and the host looks it up. A guest that asks for
	// something undeclared must reach nothing, because a compromised guest
	// otherwise turns the host end into an open proxy onto the VPN.
	g := newFakeGuest(t, "tok")
	cfg := HostConfig{
		GuestAddress: "127.0.0.1", Token: "tok", ProbeName: ProbeName,
		Upstreams: map[string]string{UpstreamKey("declared", 1433): "127.0.0.1:9"},
	}
	serveAgainst(t, g, cfg)

	conn := waitForConn(t, g)
	if _, err := conn.Write([]byte("DIAL undeclared 443\n")); err != nil {
		t.Fatal(err)
	}
	answer := readLineFrom(t, conn)
	if !strings.HasPrefix(answer, "FAIL") {
		t.Errorf("answer = %q, want a refusal for an undeclared endpoint", answer)
	}
	if !strings.Contains(answer, "undeclared") {
		t.Errorf("answer = %q, want the endpoint named", answer)
	}
}

func TestHostAnswersTheProbeItselfWithoutDiallingOut(t *testing.T) {
	// The probe must not depend on a declared dependency, or a slow endpoint
	// would look like a broken chain and a working one would mask a broken chain.
	g := newFakeGuest(t, "tok")
	cfg := HostConfig{GuestAddress: "127.0.0.1", Token: "tok", ProbeName: ProbeName}
	serveAgainst(t, g, cfg)

	conn := waitForConn(t, g)
	if _, err := conn.Write([]byte("DIAL " + ProbeName + " 1\n")); err != nil {
		t.Fatal(err)
	}
	if answer := readLineFrom(t, conn); answer != "READY" {
		t.Fatalf("answer = %q, want READY", answer)
	}
	if _, err := conn.Write([]byte("a-nonce")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("a-nonce"))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("probe got nothing back: %v", err)
	}
	if string(buf) != "a-nonce" {
		t.Errorf("probe got %q back", buf)
	}
}

func TestHostCarriesBytesToADeclaredUpstream(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 16)
		n, _ := conn.Read(buf)
		_, _ = conn.Write(append([]byte("UP:"), buf[:n]...))
	}()

	g := newFakeGuest(t, "tok")
	cfg := HostConfig{
		GuestAddress: "127.0.0.1", Token: "tok", ProbeName: ProbeName,
		Upstreams: map[string]string{UpstreamKey("vm-sql-dev", 1433): upstream.Addr().String()},
	}
	serveAgainst(t, g, cfg)

	conn := waitForConn(t, g)
	if _, err := conn.Write([]byte("DIAL vm-sql-dev 1433\n")); err != nil {
		t.Fatal(err)
	}
	if answer := readLineFrom(t, conn); answer != "READY" {
		t.Fatalf("answer = %q, want READY", answer)
	}
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("UP:hello"))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("nothing came back from the upstream: %v", err)
	}
	if string(buf) != "UP:hello" {
		t.Errorf("got %q from the upstream", buf)
	}
}

func TestHostConfigKeepsAddressesOffTheGuest(t *testing.T) {
	relays, err := Plan([]Resolved{{Name: "vm-sql-dev", Port: 1433, Address: "10.0.0.4"}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := HostConfigFor("192.168.64.8", "tok", relays)
	if err != nil {
		t.Fatal(err)
	}
	var cfg HostConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	// The address lives only here. The guest names the endpoint and this side
	// decides what that means.
	if got := cfg.Upstreams[UpstreamKey("vm-sql-dev", 1433)]; got != "10.0.0.4:1433" {
		t.Errorf("upstream = %q, want the resolved address and the real port", got)
	}
}

func waitForConn(t *testing.T, g *fakeGuest) net.Conn {
	t.Helper()
	select {
	case conn := <-g.accepted:
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	case <-time.After(3 * time.Second):
		t.Fatal("the host never connected")
		return nil
	}
}

func TestHostDoesNotTellTheGuestWhyAnUpstreamFailed(t *testing.T) {
	// The whole point of keeping addresses on this side is that the guest never
	// carries a copy of the host's private network. A dial error names the
	// address, so it must not be handed back.
	g := newFakeGuest(t, "tok")
	cfg := HostConfig{
		GuestAddress: "127.0.0.1", Token: "tok", ProbeName: ProbeName,
		// A closed port on loopback, so the dial is refused at once. An
		// unroutable address would hang for the full dial timeout instead.
		Upstreams: map[string]string{UpstreamKey("vm-sql-dev", 1433): "127.0.0.1:1"},
	}
	serveAgainst(t, g, cfg)

	conn := waitForConn(t, g)
	if _, err := conn.Write([]byte("DIAL vm-sql-dev 1433\n")); err != nil {
		t.Fatal(err)
	}
	answer := readLineFrom(t, conn)
	if !strings.HasPrefix(answer, "FAIL") {
		t.Fatalf("answer = %q, want a failure", answer)
	}
	if strings.Contains(answer, "127.0.0.1") || strings.Contains(answer, "connect") {
		t.Errorf("the guest was told where the endpoint is, or why: %q", answer)
	}
	if !strings.Contains(answer, "vm-sql-dev") {
		t.Errorf("answer = %q, want the endpoint named so the failure is actionable", answer)
	}
}

func TestHostCarriesTheWholeResponseAfterTheClientHalfCloses(t *testing.T) {
	// A client that finishes sending and half-closes must still receive the rest
	// of the response. Returning as soon as either direction ends truncates it.
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	body := strings.Repeat("payload-", 4096)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
		_, _ = conn.Write([]byte(body))
	}()

	g := newFakeGuest(t, "tok")
	cfg := HostConfig{
		GuestAddress: "127.0.0.1", Token: "tok", ProbeName: ProbeName,
		Upstreams: map[string]string{UpstreamKey("vm-sql-dev", 1433): upstream.Addr().String()},
	}
	serveAgainst(t, g, cfg)

	conn := waitForConn(t, g)
	if _, err := conn.Write([]byte("DIAL vm-sql-dev 1433\n")); err != nil {
		t.Fatal(err)
	}
	if answer := readLineFrom(t, conn); answer != "READY" {
		t.Fatalf("answer = %q, want READY", answer)
	}
	if _, err := conn.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	// Half-close, which is what makes the upstream send and what used to end the
	// splice before the response arrived.
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	if len(got) != len(body) {
		t.Errorf("got %d bytes of a %d byte response; it was truncated", len(got), len(body))
	}
}

func TestHostRefusesAnOverLongControlLine(t *testing.T) {
	// bufio grows until it finds a newline, so a peer that never sends one would
	// make this side allocate without limit.
	g := newFakeGuest(t, "tok")
	cfg := HostConfig{GuestAddress: "127.0.0.1", Token: "tok", ProbeName: ProbeName}
	serveAgainst(t, g, cfg)

	conn := waitForConn(t, g)
	flood := strings.Repeat("A", 4096)
	if _, err := conn.Write([]byte(flood)); err != nil {
		t.Fatal(err)
	}
	// The host drops the connection rather than buffering, so the read ends. How
	// it ends is not the point and varies between a clean close and a reset; a
	// timeout is the failure, because that is the host still holding the bytes.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := io.ReadAll(conn)
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Error("the host kept buffering an over-long line instead of dropping it")
	}
}

func readLineFrom(t *testing.T, conn net.Conn) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var b strings.Builder
	buf := make([]byte, 1)
	for i := 0; i < 512; i++ {
		if _, err := conn.Read(buf); err != nil {
			t.Fatalf("no answer from the host: %v", err)
		}
		if buf[0] == '\n' {
			break
		}
		b.WriteByte(buf[0])
	}
	_ = conn.SetReadDeadline(time.Time{})
	return strings.TrimSpace(b.String())
}
