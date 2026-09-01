package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdmissionRejectsOnlyImageCreatePullRoute(t *testing.T) {
	tests := []struct {
		method string
		path   string
		deny   bool
	}{
		{http.MethodPost, "/images/create?fromImage=alpine", true},
		{http.MethodPost, "/v1.51/images/create?fromImage=localhost:5000/never", true},
		{http.MethodPost, "/v1.51/images/create/", true},
		{http.MethodPost, "/v1.51//images/create", true},
		{http.MethodPost, "/v1.51/images/./create", true},
		{http.MethodPost, "/v1.51/ignored/../images/create", true},
		{http.MethodPost, "/v1.51%2fimages%2fcreate", true},
		{http.MethodPost, "/v1.51/images%2fcreate", true},
		{http.MethodPost, "/v1.51/images/%2e%2e/images/create", true},
		{http.MethodGet, "/v1.51/images/create", false},
		{http.MethodPost, "/v1.51/images/load", false},
		{http.MethodGet, "/v1.51/images/example/json", false},
		{http.MethodPost, "/v1.51/build", false},
		{http.MethodPost, "/v1.51/containers/create", false},
		{http.MethodPost, "/v1.51/containers/example/start", false},
		{http.MethodPost, "/v1.51/volumes/create", false},
		{http.MethodPost, "/v1.51/networks/create", false},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var forwarded atomic.Int32
			handler := admissionHandler(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				forwarded.Add(1)
				response.WriteHeader(http.StatusNoContent)
			}))
			request := httptest.NewRequest(tc.method, tc.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if tc.deny {
				if response.Code != http.StatusForbidden || forwarded.Load() != 0 {
					t.Fatalf("denied route code=%d forwarded=%d", response.Code, forwarded.Load())
				}
				body, _ := io.ReadAll(response.Result().Body)
				if !strings.Contains(string(body), denialMessage) {
					t.Fatalf("denial body = %q", body)
				}
				return
			}
			if response.Code != http.StatusNoContent || forwarded.Load() != 1 {
				t.Fatalf("allowed route code=%d forwarded=%d", response.Code, forwarded.Load())
			}
		})
	}
}

func TestAdmissionRejectsOpaqueBuildKitTunnels(t *testing.T) {
	for _, endpoint := range []string{"/grpc", "/session", "/v1.51/grpc", "/v1.51/session", "/v1.51//grpc", "/v1.51/ignored/../session"} {
		t.Run(endpoint, func(t *testing.T) {
			var forwarded atomic.Int32
			handler := admissionHandler(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				forwarded.Add(1)
				response.WriteHeader(http.StatusNoContent)
			}))
			request := httptest.NewRequest(http.MethodPost, endpoint, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || forwarded.Load() != 0 {
				t.Fatalf("code=%d forwarded=%d", response.Code, forwarded.Load())
			}
			if !strings.Contains(response.Body.String(), "prebakeBuilds") {
				t.Fatalf("response = %q", response.Body.String())
			}
		})
	}
}

func TestAdmissionRejectsEveryRegistryCapableEngineRoute(t *testing.T) {
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/images/search?term=alpine"},
		{http.MethodHead, "/v1.51/images/search?term=alpine"},
		{http.MethodGet, "/distribution/localhost:5000%2fnever:latest/json"},
		{http.MethodGet, "/v1.51//distribution/localhost:5000/never:latest/./json"},
		{http.MethodPost, "/auth"},
		{http.MethodPost, "/v1.51/images/localhost:5000%2fproof:latest/push"},
		{http.MethodGet, "/plugins/privileges?remote=localhost:5000/never:latest"},
		{http.MethodPost, "/plugins/pull?remote=localhost:5000/never:latest"},
		{http.MethodPost, "/plugins/example/push"},
		{http.MethodPost, "/v1.51/plugins/example/upgrade?remote=localhost:5000/never:latest"},
		{http.MethodPost, "/services/create"},
		{http.MethodPost, "/v1.51/services/example/update?version=1"},
		{http.MethodPost, "/v1.51/ignored/../services/example/update"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var forwarded atomic.Int32
			handler := admissionHandler(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				forwarded.Add(1)
				response.WriteHeader(http.StatusNoContent)
			}))
			request := httptest.NewRequest(tc.method, tc.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || forwarded.Load() != 0 {
				t.Fatalf("code=%d forwarded=%d", response.Code, forwarded.Load())
			}
			if !strings.Contains(response.Body.String(), registryDenialMessage) {
				t.Fatalf("response = %q", response.Body.String())
			}
		})
	}
}

func TestAdmissionPreservesLocalDockerOperations(t *testing.T) {
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/images/json"},
		{http.MethodGet, "/images/local:latest/json"},
		{http.MethodGet, "/images/local:latest/get"},
		{http.MethodPost, "/images/load"},
		{http.MethodPost, "/images/local:latest/tag"},
		{http.MethodPost, "/containers/create"},
		{http.MethodPost, "/containers/example/start"},
		{http.MethodPost, "/plugins/create?name=local"},
		{http.MethodPost, "/volumes/create"},
		{http.MethodPost, "/networks/create"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var forwarded atomic.Int32
			handler := admissionHandler(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				forwarded.Add(1)
				response.WriteHeader(http.StatusNoContent)
			}))
			request := httptest.NewRequest(tc.method, tc.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusNoContent || forwarded.Load() != 1 {
				t.Fatalf("code=%d forwarded=%d", response.Code, forwarded.Load())
			}
		})
	}
}

func TestRawStreamRouteMatchesOnlyHijackedEndpoints(t *testing.T) {
	tests := []struct {
		method string
		path   string
		raw    bool
	}{
		{http.MethodPost, "/containers/abc/attach", true},
		{http.MethodPost, "/v1.51/containers/abc/attach", true},
		{http.MethodPost, "/exec/abc/start", true},
		{http.MethodPost, "/v1.51/exec/abc/start", true},
		{http.MethodPost, "/v1.51/exec/abc/start/", true},
		// A real WebSocket upgrade: the reverse proxy already relays 101 correctly.
		{http.MethodGet, "/v1.51/containers/abc/attach/ws", false},
		{http.MethodGet, "/v1.51/exec/abc/start", false},
		{http.MethodPost, "/v1.51/containers/abc/wait", false},
		{http.MethodPost, "/v1.51/exec/abc/resize", false},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.path, nil)
			if got := rawStreamRoute(request.Method, request.URL); got != tc.raw {
				t.Fatalf("rawStreamRoute = %v, want %v", got, tc.raw)
			}
		})
	}
}

// A hijacked route must not become a way around admission: the path is
// canonicalized the same way for both decisions, and admission runs first.
func TestAdmissionRejectsPullSmuggledThroughAHijackedRoute(t *testing.T) {
	smuggled := []string{
		"/v1.51/exec/abc/start/../../../images/create",
		"/v1.51/containers/abc/attach/../../../images/create",
	}
	for _, path := range smuggled {
		t.Run(path, func(t *testing.T) {
			var forwarded atomic.Int32
			handler := admissionHandler(forwardHandler(
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded.Add(1) }),
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded.Add(1) }),
			))
			request := httptest.NewRequest(http.MethodPost, path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || forwarded.Load() != 0 {
				t.Fatalf("code=%d forwarded=%d", response.Code, forwarded.Load())
			}
		})
	}
}

// dockerd answers a hijacked route with 200, application/vnd.docker.raw-stream
// and no framing at all, then owns the socket. The gate must reproduce that
// exactly: a Transfer-Encoding here is what a raw-reading client decodes as
// stream payload.
func TestRawStreamTunnelForwardsHijackedResponseUnframed(t *testing.T) {
	const rawBody = "\x01\x00\x00\x00\x00\x00\x00\x03HI\n"
	const responseHead = "HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n"

	requestLines := make(chan string, 1)
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	defer backend.Close()
	go func() {
		conn, acceptErr := backend.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		line, _ := reader.ReadString('\n')
		requestLines <- strings.TrimSpace(line)
		// Drain the rest of the headers plus the JSON body before answering.
		for {
			header, headerErr := reader.ReadString('\n')
			if headerErr != nil || strings.TrimSpace(header) == "" {
				break
			}
		}
		_, _ = io.CopyN(io.Discard, reader, int64(len(`{"Detach":false,"Tty":false}`)))
		_, _ = conn.Write([]byte(responseHead + rawBody))
	}()

	gate := httptest.NewServer(forwardHandler(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("hijacked route reached the reverse proxy")
		}),
		rawStreamTunnel(func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", backend.Addr().String())
		}),
	))
	defer gate.Close()

	client, err := net.Dial("tcp", strings.TrimPrefix(gate.URL, "http://"))
	if err != nil {
		t.Fatalf("dial gate: %v", err)
	}
	defer client.Close()
	body := `{"Detach":false,"Tty":false}`
	fmt.Fprintf(client, "POST /v1.51/exec/abc/start HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)

	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("read gate response: %v", err)
	}
	if string(got) != responseHead+rawBody {
		t.Fatalf("relayed response = %q, want %q", got, responseHead+rawBody)
	}
	if line := <-requestLines; line != "POST /v1.51/exec/abc/start HTTP/1.1" {
		t.Fatalf("backend request line = %q", line)
	}
}

// The client half of a hijacked stream is stdin, and it keeps flowing after the
// request body ends.
func TestRawStreamTunnelForwardsClientBytesAfterHijack(t *testing.T) {
	relayed := make(chan string, 1)
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	defer backend.Close()
	go func() {
		conn, acceptErr := backend.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			header, headerErr := reader.ReadString('\n')
			if headerErr != nil || strings.TrimSpace(header) == "" {
				break
			}
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n"))
		stdin, _ := io.ReadAll(reader)
		relayed <- string(stdin)
	}()

	gate := httptest.NewServer(forwardHandler(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		rawStreamTunnel(func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", backend.Addr().String())
		}),
	))
	defer gate.Close()

	client, err := net.Dial("tcp", strings.TrimPrefix(gate.URL, "http://"))
	if err != nil {
		t.Fatalf("dial gate: %v", err)
	}
	defer client.Close()
	fmt.Fprint(client, "POST /v1.51/containers/abc/attach HTTP/1.1\r\nHost: docker\r\nContent-Length: 0\r\n\r\n")
	fmt.Fprint(client, "typed-after-hijack")
	if half, ok := client.(*net.TCPConn); ok {
		_ = half.CloseWrite()
	}

	select {
	case stdin := <-relayed:
		if stdin != "typed-after-hijack" {
			t.Fatalf("backend received %q", stdin)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("backend never received the post-hijack client bytes")
	}
}

// The relay measures the body it buffered, so a chunked request reaches dockerd
// with a real length and no encoding of its own.
func TestRawStreamTunnelResendsChunkedRequestWithMeasuredLength(t *testing.T) {
	const body = `{"Detach":false,"Tty":false}`
	headers := make(chan string, 1)
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	defer backend.Close()
	go func() {
		conn, acceptErr := backend.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		var head strings.Builder
		for {
			line, lineErr := reader.ReadString('\n')
			head.WriteString(line)
			if lineErr != nil || strings.TrimSpace(line) == "" {
				break
			}
		}
		sent := make([]byte, len(body))
		_, _ = io.ReadFull(reader, sent)
		head.WriteString(string(sent))
		headers <- head.String()
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
	}()

	gate := httptest.NewServer(forwardHandler(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("reached the reverse proxy") }),
		rawStreamTunnel(func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", backend.Addr().String())
		}),
	))
	defer gate.Close()

	client, err := net.Dial("tcp", strings.TrimPrefix(gate.URL, "http://"))
	if err != nil {
		t.Fatalf("dial gate: %v", err)
	}
	defer client.Close()
	fmt.Fprintf(client, "POST /v1.51/exec/abc/start HTTP/1.1\r\nHost: docker\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", len(body), body)

	select {
	case got := <-headers:
		if strings.Contains(strings.ToLower(got), "transfer-encoding") {
			t.Fatalf("relayed request kept an encoding:\n%s", got)
		}
		if !strings.Contains(got, fmt.Sprintf("Content-Length: %d", len(body))) || !strings.HasSuffix(got, body) {
			t.Fatalf("relayed request = %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("backend never received the relayed request")
	}
}

// A daemon that cannot be reached is reported in Docker's own error shape, even
// though the connection is already hijacked and net/http can no longer answer.
func TestRawStreamTunnelReportsDialFailureInDockerErrorShape(t *testing.T) {
	gate := httptest.NewServer(forwardHandler(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("reached the reverse proxy") }),
		rawStreamTunnel(func(context.Context) (net.Conn, error) {
			return nil, errors.New("no such socket")
		}),
	))
	defer gate.Close()

	client, err := net.Dial("tcp", strings.TrimPrefix(gate.URL, "http://"))
	if err != nil {
		t.Fatalf("dial gate: %v", err)
	}
	defer client.Close()
	fmt.Fprint(client, "POST /v1.51/exec/abc/start HTTP/1.1\r\nHost: docker\r\nContent-Length: 0\r\n\r\n")

	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("read gate response: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(got)), nil)
	if err != nil {
		t.Fatalf("parse relayed error %q: %v", got, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadGateway)
	}
	var payload map[string]string
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(payload["message"], "no such socket") {
		t.Fatalf("error message = %q", payload["message"])
	}
}
