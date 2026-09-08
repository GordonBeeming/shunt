// host-reach-relay is the guest end of shunt's host-reach chain. Each declared
// endpoint gets its own loopback address here; the relay accepts on it and hands
// the stream to the host, which dials the real address.
//
// It exists because the guest cannot reach those addresses itself: macOS does
// not forward the container bridge into the host's VPN tunnel, so a direct
// connect from the guest times out while the identical connect from the host
// succeeds.
//
// The host connects to this relay rather than the other way around. The macOS
// application firewall blocks incoming connections to programs it has not been
// told to allow, on every address except loopback, and blocks them without
// refusing, so a listener on the host carries nothing. Nothing filters the host
// dialling out, and nothing filters this guest accepting.
//
// One loopback address per endpoint rather than one shared address, because a
// hosts entry maps a name to an address and not to a port. Sharing one address
// would let only a single name be served per port, and a real project declares
// many names on 443.
//
// The wire protocol is line-oriented text, and its verbs are the only thing this
// program and the host end both have to know; everything else travels in the
// config. The host end is internal/hostreach/host.go.
//
//	host -> guest   SHUNT1 <token>        on connect
//	guest -> host   OK                    token accepted, connection pooled
//	guest -> host   DIAL <name> <port>    when an app connection needs it
//	host -> guest   READY                 upstream is connected, splice
//	host -> guest   FAIL <reason>         upstream refused
package main

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// checkTimeout bounds the reachability check and the per-request exchange with
// the host. Both ends are on one machine, so a slow answer means the connection
// is not being carried rather than that the network is far away.
const checkTimeout = 3 * time.Second

// handshakeTimeout bounds the token exchange. A connection that does not
// authenticate promptly is not the host, so it is dropped rather than left
// holding a slot.
const handshakeTimeout = 5 * time.Second

// dialWait is how long an app connection waits for a pooled host connection when
// the pool is momentarily empty. The host refills continuously, so this covers a
// burst rather than an outage.
const dialWait = 30 * time.Second

// requestTimeout bounds the wait for the host's answer to a DIAL. It has to
// exceed the host's own dial timeout: an upstream across a VPN can take seconds
// to connect, and a shorter wait here discards a connection the host is about to
// answer on, then retries into the same wall.
const requestTimeout = 20 * time.Second

// idleDepth caps the pooled connections. A host that over-supplies cannot grow
// this without bound.
const idleDepth = 64

// maxLine bounds a control line. bufio's ReadString grows until it finds a
// newline, so a peer that never sends one would make this side allocate without
// limit. Every line in this protocol is short.
const maxLine = 512

// readLine reads one line and refuses anything longer than maxLine, through the
// connection's shared reader so bytes arriving with the line are kept.
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

// entry is one relayed endpoint, written by shunt after the guest starts.
type entry struct {
	Name         string `json:"name"`
	GuestAddress string `json:"guestAddress"`
	Port         int    `json:"port"`
}

type config struct {
	ControlPort int     `json:"controlPort"`
	Token       string  `json:"token"`
	ProbeName   string  `json:"probeName"`
	Entries     []entry `json:"entries"`
}

// hostConn is a pooled connection and the reader that owns its buffered bytes.
//
// The reader is created once and travels with the connection. A reader created
// per step buffers whatever arrived alongside the line it was asked for, and
// those bytes are lost when it goes out of scope: the host's READY can arrive in
// the same segment as the upstream's first bytes.
type hostConn struct {
	conn   net.Conn
	reader *bufio.Reader
}

// pool holds authenticated connections the host has opened and not yet been
// given work on.
type pool struct {
	idle chan *hostConn
}

func newPool() *pool { return &pool{idle: make(chan *hostConn, idleDepth)} }

func (p *pool) put(hc *hostConn) {
	select {
	case p.idle <- hc:
	default:
		_ = hc.conn.Close()
	}
}

func (p *pool) take(wait time.Duration) (*hostConn, error) {
	if wait <= 0 {
		return nil, errors.New("no connection from the host is available")
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case hc := <-p.idle:
		return hc, nil
	case <-timer.C:
		return nil, errors.New("no connection from the host is available")
	}
}

func main() {
	configPath := flag.String("config", "", "path to the relay configuration shunt wrote")
	check := flag.String("check", "", "send --nonce to this address, require it back, and exit")
	nonce := flag.String("nonce", "", "the value --check sends and requires back")
	flag.Parse()
	if *check != "" {
		runCheck(*check, *nonce)
		return
	}
	if *configPath == "" {
		log.Fatal("host-reach-relay: --config is required")
	}
	cfg, err := readConfig(*configPath)
	if err != nil {
		log.Fatalf("host-reach-relay: %v", err)
	}
	if len(cfg.Entries) == 0 {
		// Nothing declared is a normal state, not a failure. Exiting quietly keeps
		// the entrypoint's start unconditional.
		return
	}

	p := newPool()
	control, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.ControlPort))
	if err != nil {
		log.Fatalf("host-reach-relay: listen for the host on port %d: %v", cfg.ControlPort, err)
	}
	log.Printf("waiting for the host on port %d", cfg.ControlPort)
	go acceptHost(control, cfg.Token, p)

	var wg sync.WaitGroup
	for _, e := range cfg.Entries {
		listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", e.GuestAddress, e.Port))
		if err != nil {
			// Fail the whole relay rather than serving a subset: a half-built relay
			// gives some names a connection and others a timeout, which reads as an
			// application bug rather than a configuration one.
			log.Fatalf("host-reach-relay: listen for %s on %s:%d: %v", e.Name, e.GuestAddress, e.Port, err)
		}
		log.Printf("relaying %s (%s:%d) through the host", e.Name, e.GuestAddress, e.Port)
		wg.Add(1)
		go func(e entry, l net.Listener) {
			defer wg.Done()
			serve(e, l, p)
		}(e, listener)
	}
	wg.Wait()
}

// acceptHost takes the host's connections, checks the token, and pools them.
//
// The token check comes before anything else this does with a connection. Every
// guest on the bridge can reach this port, and a connection pooled without one
// would be handed an application's traffic.
func acceptHost(listener net.Listener, token string, p *pool) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("host-reach-relay: accept from the host: %v", err)
			continue
		}
		go func(conn net.Conn) {
			reader := bufio.NewReader(conn)
			if err := authenticate(conn, reader, token); err != nil {
				log.Printf("host-reach-relay: rejected a connection from %s: %v", conn.RemoteAddr(), err)
				_ = conn.Close()
				return
			}
			p.put(&hostConn{conn: conn, reader: reader})
		}(conn)
	}
}

func authenticate(conn net.Conn, reader *bufio.Reader, token string) error {
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return err
	}
	line, err := readLine(reader)
	if err != nil {
		return fmt.Errorf("read the greeting: %w", err)
	}
	name, given, found := strings.Cut(strings.TrimSpace(line), " ")
	if !found || name != "SHUNT1" {
		return errors.New("not a shunt host-reach greeting")
	}
	if subtle.ConstantTimeCompare([]byte(given), []byte(token)) != 1 {
		return errors.New("wrong token")
	}
	if _, err := conn.Write([]byte("OK\n")); err != nil {
		return err
	}
	// Cleared so a pooled connection can wait indefinitely for work.
	return conn.SetDeadline(time.Time{})
}

// runCheck proves the guest can carry traffic to the host end, not merely open a
// connection to it. It runs here rather than on the host because this is the
// path the relay actually uses, and it is the only path nothing else exercises.
//
// A dial proves nothing on its own. A blocked or unattended listener still
// completes the handshake in the kernel, so the caller sees a dial succeed and a
// read that never returns.
func runCheck(target, nonce string) {
	if nonce == "" {
		log.Fatal("host-reach-relay: --check needs --nonce")
	}
	conn, err := net.DialTimeout("tcp", target, checkTimeout)
	if err != nil {
		log.Fatalf("host-reach-relay: dial %s: %v", target, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(checkTimeout)); err != nil {
		log.Fatalf("host-reach-relay: %v", err)
	}
	if _, err := conn.Write([]byte(nonce)); err != nil {
		log.Fatalf("host-reach-relay: write to %s: %v", target, err)
	}
	buf := make([]byte, len(nonce))
	if _, err := io.ReadFull(conn, buf); err != nil {
		log.Fatalf("host-reach-relay: %s accepted the connection but returned nothing: %v", target, err)
	}
	if string(buf) != nonce {
		log.Fatalf("host-reach-relay: %s answered with something other than the check value", target)
	}
}

func readConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config{}, fmt.Errorf("read relay config: %w", err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return config{}, fmt.Errorf("parse relay config: %w", err)
	}
	if cfg.ControlPort == 0 || cfg.Token == "" {
		return config{}, errors.New("relay config has no control port or token")
	}
	for _, e := range cfg.Entries {
		if e.Name == "" || e.GuestAddress == "" || e.Port == 0 {
			return config{}, fmt.Errorf("relay config entry %q is incomplete", e.Name)
		}
	}
	return cfg, nil
}

func serve(e entry, listener net.Listener, p *pool) {
	for {
		client, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("host-reach-relay: accept for %s: %v", e.Name, err)
			continue
		}
		go forward(e, client, p)
	}
}

func forward(e entry, client net.Conn, p *pool) {
	defer client.Close()
	hc, err := reserve(e, p)
	if err != nil {
		// Naming the endpoint matters more than the address: the address is an
		// internal hop, and the reader is looking for which dependency is down.
		log.Printf("host-reach-relay: %s: %v", e.Name, err)
		return
	}
	defer hc.conn.Close()

	done := make(chan struct{}, 2)
	// Read the host's side through its own reader, which may already hold bytes
	// that arrived with the READY line.
	go copyFrom(client, hc.reader, done)
	go copyStream(hc.conn, client, done)
	// Both directions. Each half closes the peer's write side when it ends, so
	// the other sees EOF and finishes; returning after the first would let the
	// deferred closes truncate a response the application is still owed.
	<-done
	<-done
}

// reserve takes a pooled host connection and asks it for this endpoint.
//
// A pooled connection can be stale: the host process may have gone away without
// the guest noticing, and that is only discovered on use. Each attempt discards
// the connection it failed on and tries another rather than giving up.
func reserve(e entry, p *pool) (*hostConn, error) {
	deadline := time.Now().Add(dialWait)
	var last error
	for {
		hc, err := p.take(time.Until(deadline))
		if err != nil {
			if last != nil {
				return nil, last
			}
			return nil, err
		}
		if err := request(hc, e); err != nil {
			_ = hc.conn.Close()
			last = err
			continue
		}
		return hc, nil
	}
}

func request(hc *hostConn, e entry) error {
	if err := hc.conn.SetDeadline(time.Now().Add(requestTimeout)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(hc.conn, "DIAL %s %d\n", e.Name, e.Port); err != nil {
		return fmt.Errorf("ask the host for %s: %w", e.Name, err)
	}
	line, err := readLine(hc.reader)
	if err != nil {
		return fmt.Errorf("no answer from the host for %s: %w", e.Name, err)
	}
	answer := strings.TrimSpace(line)
	if answer != "READY" {
		return fmt.Errorf("the host could not reach %s: %s", e.Name, strings.TrimPrefix(answer, "FAIL "))
	}
	return hc.conn.SetDeadline(time.Time{})
}

// copyFrom is copyStream reading through a buffered reader rather than straight
// from the connection, so bytes already buffered are not left behind.
func copyFrom(dst net.Conn, src io.Reader, done chan<- struct{}) {
	_, _ = io.Copy(dst, src)
	if closer, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = closer.CloseWrite()
	}
	done <- struct{}{}
}

// copyStream moves bytes one way and signals when that direction ends. Either
// half closing ends the pair, so a peer that hangs up cannot strand the other
// side holding a connection open.
func copyStream(dst, src net.Conn, done chan<- struct{}) {
	_, _ = io.Copy(dst, src)
	if closer, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = closer.CloseWrite()
	}
	done <- struct{}{}
}
