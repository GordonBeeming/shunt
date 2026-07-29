package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
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
