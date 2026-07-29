// docker-api-admission is the in-guest Docker API gate. It forwards the
// complete Docker HTTP API to a private dockerd socket except image-create,
// which is the API operation used by docker pull (including loopback pulls),
// and builds whose Dockerfile can reference an image not already loaded.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", backendPath)
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

	server := &http.Server{
		Handler:           admissionHandler(buildAdmissionHandler(proxy, backendImageInspector(backendClient))),
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
