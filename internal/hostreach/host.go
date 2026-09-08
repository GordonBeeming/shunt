package hostreach

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// The host end of the chain. It dials the guest and waits to be given work,
// rather than listening for the guest to connect.
//
// That direction is forced: the macOS application firewall blocks incoming
// connections to programs it has not been told to allow, on every address except
// loopback, and blocks them without refusing, so a listener here would complete
// the handshake and then carry nothing. Nothing filters dialling out.
//
// The protocol's verbs are shared with the guest relay, which lives at
// internal/image/assets/hostreach/relay.go and cannot import this package
// because it is built into the guest image as its own program.

const (
	// poolDepth is how many connections are kept waiting for work in the guest.
	// Enough that a burst of application connections is served without a round
	// trip; small enough to be cheap to hold open.
	poolDepth = 8

	// dialTimeout bounds both the connection to the guest and the connection to
	// an upstream. The guest is on this machine; an upstream is across a VPN, so
	// this is generous rather than tight.
	dialTimeout = 15 * time.Second

	// retryFloor and retryCeiling bound the wait between attempts to reach a
	// guest. A guest restarts, and the host end should notice quickly at first
	// and then stop hammering a guest that is gone.
	retryFloor   = 250 * time.Millisecond
	retryCeiling = 10 * time.Second
)

// HostConfig is what shunt writes for the host end. Endpoints are keyed by the
// name and port the guest asks for, so the host never trusts the guest to supply
// an address: a guest asking for something undeclared is refused.
type HostConfig struct {
	GuestAddress string            `json:"guestAddress"`
	Token        string            `json:"token"`
	ProbeName    string            `json:"probeName"`
	Upstreams    map[string]string `json:"upstreams"`
}

// maxLine bounds a control line. bufio's ReadString grows until it finds a
// newline, so a peer that never sends one would make this side allocate without
// limit. Every line in this protocol is short.
const maxLine = 512

// readLine reads one line and refuses anything longer than maxLine. It reads
// through the connection's shared reader, so bytes that arrive with the line are
// kept for whatever reads next.
func readLine(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for i := 0; i < maxLine; i++ {
		c, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if c == '\n' {
			return b.String(), nil
		}
		b.WriteByte(c)
	}
	return "", errors.New("the peer sent an over-long line")
}

// UpstreamKey names an endpoint in the host config.
func UpstreamKey(name string, port int) string {
	return name + ":" + strconv.Itoa(port)
}

// HostConfigFor renders the host end's configuration for a planned siding.
func HostConfigFor(guestAddress, token string, relays []Relay) ([]byte, error) {
	upstreams := make(map[string]string, len(relays))
	for _, r := range relays {
		upstreams[UpstreamKey(r.Name, r.Port)] = net.JoinHostPort(r.Address, strconv.Itoa(r.Port))
	}
	return json.MarshalIndent(HostConfig{
		GuestAddress: guestAddress,
		Token:        token,
		ProbeName:    ProbeName,
		Upstreams:    upstreams,
	}, "", "  ")
}

// LoadHostConfig reads a host config written by HostConfigFor.
func LoadHostConfig(path string) (HostConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return HostConfig{}, fmt.Errorf("read the host-reach config: %w", err)
	}
	var cfg HostConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return HostConfig{}, fmt.Errorf("parse the host-reach config: %w", err)
	}
	if cfg.GuestAddress == "" || cfg.Token == "" {
		return HostConfig{}, errors.New("the host-reach config has no guest address or token")
	}
	return cfg, nil
}

// Serve keeps poolDepth connections waiting in the guest and services each one
// as the guest hands it work. It returns only when ctx is done.
func Serve(ctx context.Context, cfg HostConfig, logger *log.Logger) error {
	target, err := GuestControlAddress(cfg.GuestAddress)
	if err != nil {
		return err
	}
	logger.Printf("serving host-reach for the guest at %s", target)
	for i := 0; i < poolDepth; i++ {
		go holdOne(ctx, target, cfg, logger)
	}
	<-ctx.Done()
	return nil
}

// holdOne keeps exactly one connection in the guest's pool: it connects, waits
// for work, does it, and connects again. poolDepth of these give the pool its
// depth without anything having to count what is in it.
func holdOne(ctx context.Context, target string, cfg HostConfig, logger *log.Logger) {
	wait := retryFloor
	for ctx.Err() == nil {
		if err := serveOne(ctx, target, cfg); err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Printf("host-reach: %v", err)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
			// Back off towards the ceiling so a guest that has gone away is not
			// hammered, and reset on the next success.
			if wait *= 2; wait > retryCeiling {
				wait = retryCeiling
			}
			continue
		}
		wait = retryFloor
	}
}

func serveOne(ctx context.Context, target string, cfg HostConfig) error {
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return fmt.Errorf("connect to the guest at %s: %w", target, err)
	}
	defer conn.Close()

	// One reader for the whole connection. A reader created per step buffers
	// whatever arrived with the line it was asked for, and those bytes are lost
	// when it goes out of scope: the guest's request can arrive in the same
	// segment as its greeting, and an application's first bytes can arrive with
	// its request.
	reader := bufio.NewReader(conn)
	if err := greet(conn, reader, cfg.Token); err != nil {
		return err
	}

	// No deadline while waiting: a pooled connection is idle until an application
	// in the guest needs it, which may be hours.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return err
	}
	line, err := readLine(reader)
	if err != nil {
		if errors.Is(err, io.EOF) {
			// The guest closed an idle connection, most likely because its relay
			// restarted. Reconnecting is the whole response.
			return nil
		}
		return fmt.Errorf("wait for work from the guest: %w", err)
	}
	return handle(ctx, conn, reader, cfg, strings.TrimSpace(line))
}

func greet(conn net.Conn, reader *bufio.Reader, token string) error {
	if err := conn.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(conn, "SHUNT1 %s\n", token); err != nil {
		return fmt.Errorf("greet the guest: %w", err)
	}
	line, err := readLine(reader)
	if err != nil {
		return fmt.Errorf("read the guest's answer: %w", err)
	}
	if strings.TrimSpace(line) != "OK" {
		return fmt.Errorf("the guest refused the token: %s", strings.TrimSpace(line))
	}
	return nil
}

// handle services one request. The guest names an endpoint; the host decides
// what that means, because the address never leaves this side.
func handle(ctx context.Context, conn net.Conn, reader *bufio.Reader, cfg HostConfig, request string) error {
	verb, rest, found := strings.Cut(request, " ")
	if !found || verb != "DIAL" {
		_, _ = fmt.Fprintf(conn, "FAIL %s is not a request\n", verb)
		return fmt.Errorf("the guest sent %q", request)
	}
	name, portText, found := strings.Cut(rest, " ")
	if !found {
		_, _ = fmt.Fprintln(conn, "FAIL malformed request")
		return fmt.Errorf("the guest sent %q", request)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		_, _ = fmt.Fprintln(conn, "FAIL malformed port")
		return fmt.Errorf("the guest sent %q", request)
	}

	if name == cfg.ProbeName {
		if _, err := fmt.Fprintln(conn, "READY"); err != nil {
			return err
		}
		return echo(conn, reader)
	}

	// Looked up rather than taken from the request. The guest never supplies an
	// address, so a guest asking for something undeclared reaches nothing.
	upstream, ok := cfg.Upstreams[UpstreamKey(name, port)]
	if !ok {
		_, _ = fmt.Fprintf(conn, "FAIL %s is not declared\n", name)
		return fmt.Errorf("the guest asked for undeclared endpoint %s:%d", name, port)
	}

	// A fresh unbound socket, so the kernel picks the source by route. A socket
	// bound to the container bridge is dropped by the VPN, and this is the dial
	// that has to reach the endpoint.
	dialer := net.Dialer{Timeout: dialTimeout}
	target, err := dialer.DialContext(ctx, "tcp", upstream)
	if err != nil {
		// Named, not described. The dial error carries the private address, and
		// the guest is deliberately never told it; the detail is logged here.
		_, _ = fmt.Fprintf(conn, "FAIL could not reach %s\n", name)
		return fmt.Errorf("dial %s for %s: %w", upstream, name, err)
	}
	defer target.Close()

	if _, err := fmt.Fprintln(conn, "READY"); err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return err
	}
	splice(conn, reader, target)
	return nil
}

// echo answers the probe from here rather than dialling out, so a check
// exercises the guest's listener, the pool, the token and this process without
// touching a real dependency.
func echo(conn net.Conn, reader *bufio.Reader) error {
	if err := conn.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
		return err
	}
	_, err := io.Copy(conn, reader)
	return err
}

// splice joins the guest's connection to the upstream. The buffered reader is
// drained first: it may already hold bytes the application sent immediately
// after the request, and reading from the raw connection would lose them.
func splice(conn net.Conn, reader *bufio.Reader, target net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(target, reader)
		if closer, ok := target.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, target)
		if closer, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		done <- struct{}{}
	}()
	// Both directions. Each half closes the peer's write side when it ends, so
	// the other sees EOF and finishes; returning after the first would let the
	// deferred closes truncate a response the client is still owed.
	<-done
	<-done
}
