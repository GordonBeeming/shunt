// host-reach-relay is the guest end of shunt's host-reach chain. Each declared
// endpoint gets its own loopback address here; the relay accepts on it and
// forwards to a port on the container bridge, where the host end dials the real
// address.
//
// It exists because the guest cannot reach those addresses itself: macOS does
// not forward the bridge into the host's VPN tunnel, so a direct connect from
// the guest times out while the identical connect from the host succeeds.
//
// One loopback address per endpoint rather than one shared address, because a
// hosts entry maps a name to an address and not to a port. Sharing one address
// would let only a single name be served per port, and a real project declares
// many names on 443.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

// dialTimeout bounds a connection to the bridge. The host end is local, so a
// slow dial means the listener is gone rather than the network being far away.
const dialTimeout = 10 * time.Second

// checkTimeout bounds the reachability check. Both ends are on one machine, so a
// slow answer means the connection is not being carried rather than that the
// network is far away.
const checkTimeout = 3 * time.Second

// entry is one relayed endpoint, written by shunt after the guest starts.
type entry struct {
	Name         string `json:"name"`
	GuestAddress string `json:"guestAddress"`
	Port         int    `json:"port"`
	BridgeTarget string `json:"bridgeTarget"`
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
	entries, err := readConfig(*configPath)
	if err != nil {
		log.Fatalf("host-reach-relay: %v", err)
	}
	if len(entries) == 0 {
		// Nothing declared is a normal state, not a failure. Exiting quietly keeps
		// the entrypoint's start unconditional.
		return
	}

	var wg sync.WaitGroup
	for _, e := range entries {
		listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", e.GuestAddress, e.Port))
		if err != nil {
			// Fail the whole relay rather than serving a subset: a half-built relay
			// gives some names a connection and others a timeout, which reads as an
			// application bug rather than a configuration one.
			log.Fatalf("host-reach-relay: listen for %s on %s:%d: %v", e.Name, e.GuestAddress, e.Port, err)
		}
		log.Printf("relaying %s (%s:%d) to %s", e.Name, e.GuestAddress, e.Port, e.BridgeTarget)
		wg.Add(1)
		go func(e entry, l net.Listener) {
			defer wg.Done()
			serve(e, l)
		}(e, listener)
	}
	wg.Wait()
}

// runCheck proves the guest can carry traffic to the host end, not merely open a
// connection to it. It runs here rather than on the host because this is the
// path the relay actually uses, and it is the only path nothing else exercises.
//
// A dial proves nothing. The macOS application firewall blocks inbound
// connections to an unapproved program on any address except loopback, and
// blocks them after the handshake: the kernel completes the connection, the
// program never accepts, and the caller sees a dial succeed and a read that
// never returns.
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

func readConfig(path string) ([]entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read relay config: %w", err)
	}
	var entries []entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse relay config: %w", err)
	}
	for _, e := range entries {
		if e.GuestAddress == "" || e.Port == 0 || e.BridgeTarget == "" {
			return nil, fmt.Errorf("relay config entry %q is incomplete", e.Name)
		}
	}
	return entries, nil
}

func serve(e entry, listener net.Listener) {
	for {
		client, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("host-reach-relay: accept for %s: %v", e.Name, err)
			continue
		}
		go forward(e, client)
	}
}

func forward(e entry, client net.Conn) {
	defer client.Close()
	upstream, err := net.DialTimeout("tcp", e.BridgeTarget, dialTimeout)
	if err != nil {
		// Naming the endpoint matters more than the address: the address is an
		// internal hop, and the reader is looking for which dependency is down.
		log.Printf("host-reach-relay: dial %s for %s: %v", e.BridgeTarget, e.Name, err)
		return
	}
	defer upstream.Close()

	done := make(chan struct{}, 2)
	go copyStream(upstream, client, done)
	go copyStream(client, upstream, done)
	<-done
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
