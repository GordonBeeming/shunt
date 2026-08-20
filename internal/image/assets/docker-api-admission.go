// docker-api-admission is the in-guest Docker API gate. It forwards the
// complete Docker HTTP API to a private dockerd socket except routes that can
// pull images or access registries, opaque BuildKit tunnels, and builds whose
// Dockerfile can reference an image not already loaded.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const denialMessage = "shunt offline policy rejects Docker image pulls"

const buildTunnelDenialMessage = "shunt offline policy blocks BuildKit/buildx tunnels; configure host prebakeBuilds instead"

const registryDenialMessage = "shunt offline policy rejects Docker registry access"

// maxHijackBodyBytes bounds the options object buffered ahead of a stream
// hijack. Docker's attach and exec-start bodies are a handful of booleans.
const maxHijackBodyBytes = 1 << 20

var dockerVersionPrefix = regexp.MustCompile(`^/v[0-9]+(?:\.[0-9]+)?`)

func main() {
	listenPath := flag.String("listen", "/var/run/docker.sock", "public Docker API Unix socket")
	backendPath := flag.String("backend", "/run/shunt/dockerd/docker.sock", "private dockerd Unix socket")
	flag.Parse()
	if err := serve(*listenPath, *backendPath); err != nil {
		fmt.Fprintf(os.Stderr, "shunt-docker-api-admission: %v\n", err)
		os.Exit(1)
	}
}

func serve(listenPath, backendPath string) error {
	if listenPath == backendPath {
		return fmt.Errorf("listen and backend sockets must differ")
	}
	if err := os.MkdirAll(filepath.Dir(listenPath), 0o755); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	if err := removeSocket(listenPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", listenPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listenPath, err)
	}
	defer listener.Close()
	defer os.Remove(listenPath)
	if err := os.Chmod(listenPath, 0o660); err != nil {
		return fmt.Errorf("chmod %s: %w", listenPath, err)
	}

	dialBackend := func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", backendPath)
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialBackend(ctx)
		},
		DisableCompression: true,
	}
	backendURL := &url.URL{Scheme: "http", Host: "dockerd"}
	proxy := httputil.NewSingleHostReverseProxy(backendURL)
	proxy.Transport = transport
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(response http.ResponseWriter, request *http.Request, err error) {
		writeDockerError(response, http.StatusBadGateway, fmt.Sprintf("Docker daemon unavailable: %v", err))
	}
	backendClient := &http.Client{Transport: transport, Timeout: 15 * time.Second}

	forwarder := forwardHandler(proxy, rawStreamTunnel(dialBackend))
	server := &http.Server{
		Handler:           admissionHandler(buildAdmissionHandler(forwarder, backendImageInspector(backendClient))),
		ReadHeaderTimeout: 15 * time.Second,
	}
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func admissionHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if rejectsPull(request.Method, request.URL) {
			writeDockerError(response, http.StatusForbidden, denialMessage)
			return
		}
		if rejectsOpaqueBuildTunnel(request.Method, request.URL) {
			writeDockerError(response, http.StatusForbidden, buildTunnelDenialMessage)
			return
		}
		if rejectsRegistryOperation(request.Method, request.URL) {
			writeDockerError(response, http.StatusForbidden, registryDenialMessage)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func rejectsRegistryOperation(method string, requestURL *url.URL) bool {
	requestPath := dockerAPIPath(requestURL)
	readMethod := method == http.MethodGet || method == http.MethodHead
	if readMethod {
		if requestPath == "/images/search" || requestPath == "/plugins/privileges" {
			return true
		}
		if isDockerSubresource(requestPath, "/distribution/", "/json") {
			return true
		}
	}
	if method != http.MethodPost {
		return false
	}
	if requestPath == "/auth" || requestPath == "/plugins/pull" || requestPath == "/services/create" {
		return true
	}
	return isDockerSubresource(requestPath, "/images/", "/push") ||
		isDockerSubresource(requestPath, "/plugins/", "/push") ||
		isDockerSubresource(requestPath, "/plugins/", "/upgrade") ||
		isDockerSubresource(requestPath, "/services/", "/update")
}

func isDockerSubresource(requestPath, prefix, suffix string) bool {
	if !strings.HasPrefix(requestPath, prefix) || !strings.HasSuffix(requestPath, suffix) {
		return false
	}
	middle := strings.TrimSuffix(strings.TrimPrefix(requestPath, prefix), suffix)
	return middle != ""
}

func rejectsOpaqueBuildTunnel(method string, requestURL *url.URL) bool {
	return isDockerEndpoint(method, requestURL, "/grpc") || isDockerEndpoint(method, requestURL, "/session")
}

func rejectsPull(method string, requestURL *url.URL) bool {
	// net/http exposes a decoded URL.Path and ReverseProxy preserves RawPath
	// when it is a valid encoding. Canonicalize the decoded path before matching
	// so double slashes, dot segments, and escaped separators cannot become an
	// image-create route only after the request crosses the admission boundary.
	return isDockerEndpoint(method, requestURL, "/images/create")
}

// forwardHandler picks how an admitted request reaches dockerd. Everything that
// speaks ordinary HTTP goes through the reverse proxy; the routes dockerd
// answers by seizing the connection are tunnelled raw instead.
func forwardHandler(proxy, tunnel http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if rawStreamRoute(request.Method, request.URL) {
			tunnel.ServeHTTP(response, request)
			return
		}
		proxy.ServeHTTP(response, request)
	})
}

// rawStreamRoute reports the routes dockerd answers with a hijacked connection
// rather than a framed HTTP body: 200 plus application/vnd.docker.raw-stream,
// no Content-Length, raw bytes until close. /containers/{id}/attach/ws is
// deliberately absent, being a real WebSocket upgrade that the reverse proxy
// already relays correctly.
func rawStreamRoute(method string, requestURL *url.URL) bool {
	if method != http.MethodPost {
		return false
	}
	requestPath := dockerAPIPath(requestURL)
	return isDockerSubresource(requestPath, "/containers/", "/attach") ||
		isDockerSubresource(requestPath, "/exec/", "/start")
}

// rawStreamTunnel relays a request over a dedicated backend connection, byte for
// byte in both directions.
//
// httputil.ReverseProxy hands the connection over only for a 101 upgrade, and
// dockerd answers 101 only when the client asked for one. A client that omits
// the upgrade headers gets dockerd's other hijack style instead: 200 with an
// unframed raw body. The proxy cannot recognize that, so Go re-frames it as
// chunked on the way out, and any client reading the stream raw (Docker.DotNet,
// and through it Testcontainers) decodes the chunk headers as stream payload.
// Copying the bytes untouched is correct for both styles at once.
func rawStreamTunnel(dial func(context.Context) (net.Conn, error)) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		hijacker, ok := response.(http.Hijacker)
		if !ok {
			writeDockerError(response, http.StatusInternalServerError, "Docker stream relay unsupported on this connection")
			return
		}
		// The body has to be captured before the hijack, because reading
		// request.Body needs the connection net/http is about to hand over.
		// These routes carry a small JSON options object; the cap only stops an
		// unbounded read.
		body, err := io.ReadAll(io.LimitReader(request.Body, maxHijackBodyBytes+1))
		if err != nil {
			writeDockerError(response, http.StatusBadRequest, fmt.Sprintf("read Docker stream request: %v", err))
			return
		}
		if len(body) > maxHijackBodyBytes {
			writeDockerError(response, http.StatusRequestEntityTooLarge, "Docker stream request body too large")
			return
		}

		// Hijack before dialing: from here net/http no longer manages this
		// connection, so it can neither cancel the request context when the
		// client half-closes stdin, which attach clients do routinely with
		// stdout still to come, nor mistake post-hijack stream bytes for a
		// pipelined request.
		client, buffered, err := hijacker.Hijack()
		if err != nil {
			writeDockerError(response, http.StatusInternalServerError, fmt.Sprintf("take over Docker stream connection: %v", err))
			return
		}
		defer client.Close()

		// Detached from the request context for the same reason: its cancellation
		// now tracks a connection this handler owns, not the daemon dial.
		backend, err := dial(context.Background())
		if err != nil {
			writeHijackedDockerError(client, http.StatusBadGateway, fmt.Sprintf("Docker daemon unavailable: %v", err))
			return
		}
		defer backend.Close()

		// Host is empty on a server-side request and Request.Write needs one to
		// emit a Host header.
		if request.Host == "" {
			request.Host = "dockerd"
		}
		// Replay the buffered body at a known length. http.NoBody is the only way
		// to say "definitely empty": a zero ContentLength with a non-nil Body
		// reads as unknown length, and Request.Write would chunk-encode it, which
		// is the very framing this relay exists to avoid. A chunked inbound
		// request is re-sent with its measured length, dropping the encoding too.
		request.TransferEncoding = nil
		request.ContentLength = int64(len(body))
		if len(body) == 0 {
			request.Body = http.NoBody
		} else {
			request.Body = io.NopCloser(bytes.NewReader(body))
		}
		if err := request.Write(backend); err != nil {
			writeHijackedDockerError(client, http.StatusBadGateway, fmt.Sprintf("Docker daemon unavailable: %v", err))
			return
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = io.Copy(client, backend)
			closeWrite(client)
		}()
		// Read the client side through the hijack reader, not the bare socket:
		// it holds whatever the server buffered past the request headers.
		_, _ = io.Copy(backend, buffered)
		closeWrite(backend)
		<-done
	})
}

// writeHijackedDockerError reports a failure that happens once the connection is
// already hijacked, where net/http can no longer render a response. The JSON
// shape matches writeDockerError so clients see one error contract either way.
func writeHijackedDockerError(conn net.Conn, status int, message string) {
	payload, err := json.Marshal(map[string]string{"message": message})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(payload), payload)
}

// closeWrite half-closes so the peer sees EOF on its read side while the other
// direction keeps streaming. A stream whose stdin has ended still has stdout to
// deliver.
func closeWrite(conn net.Conn) {
	if half, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()
	}
}

func writeDockerError(response http.ResponseWriter, status int, message string) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]string{"message": message})
}

func removeSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to replace non-socket %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	return nil
}
