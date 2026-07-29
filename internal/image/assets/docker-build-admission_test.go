package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestDockerfileImageSourcesUsesBuildKitParser(t *testing.T) {
	dockerfile := []byte(`ARG BASE=local/base:1
FROM ${BASE} AS compile
RUN --mount=type=bind,from=tools/image:2,source=/bin,target=/tools true
COPY --from=copy/image:3 /source /dest
ADD local.tar /tmp/
FROM compile AS final
COPY --from=0 /dest /dest
`)
	wanted := []string{"copy/image:3", "local/base:1", "tools/image:2"}
	got, err := dockerfileImageSources(dockerfile, map[string]*string{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wanted) {
		t.Fatalf("sources = %#v, want %#v", got, wanted)
	}
}

func TestDockerfileImageSourcesIncludesSyntaxFrontendAndBuildArgOverride(t *testing.T) {
	override := "loaded/base:override"
	dockerfile := []byte(`# syntax=docker/dockerfile:1.20
ARG BASE=default/base:1
FROM ${BASE}
`)
	wanted := []string{"docker/dockerfile:1.20", override}
	got, err := dockerfileImageSources(dockerfile, map[string]*string{"BASE": &override})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wanted) {
		t.Fatalf("sources = %#v, want %#v", got, wanted)
	}
}

func TestDockerfileImageSourcesFailsClosed(t *testing.T) {
	tests := map[string]string{
		"unresolved FROM":   "FROM ${MISSING}\n",
		"external HTTP ADD": "FROM scratch\nADD https://example.invalid/file /tmp/\n",
		"external git ADD":  "FROM scratch\nADD git@example.invalid:repo.git /tmp/\n",
		"external ssh ADD":  "FROM scratch\nADD deploy@example.invalid:repo.git /tmp/\n",
		"ONBUILD":           "FROM loaded/base:1\nONBUILD COPY --from=remote/image:1 /x /y\n",
	}
	for name, dockerfile := range tests {
		t.Run(name, func(t *testing.T) {
			if sources, err := dockerfileImageSources([]byte(dockerfile), nil); err == nil {
				t.Fatalf("sources = %#v, want rejection", sources)
			}
		})
	}
}

func TestRejectsBuildOptionsThatCanIntroduceSources(t *testing.T) {
	tests := []string{
		"remote=https%3A%2F%2Fexample.invalid%2Fcontext.tar",
		"remote=&remote=https%3A%2F%2Fexample.invalid%2Fcontext.tar",
		"remote=&remote=",
		"session=session-id",
		"cachefrom=%5B%22registry.invalid%2Fcache%22%5D",
		"cachefrom=&cachefrom=%5B%22registry.invalid%2Fcache%22%5D",
		"cache-from=type%3Dregistry%2Cref%3Dregistry.invalid%2Fcache",
		"cacheto=type%3Dregistry%2Cref%3Dregistry.invalid%2Fcache",
		"outputs=type%3Dregistry",
		"outputs=&outputs=type%3Dregistry",
		"pull=1",
		"pull=true",
		"pull=perhaps",
		"pull=0&pull=0",
		"dockerfile=Dockerfile&dockerfile=Otherfile",
		"buildargs=%7B%7D&buildargs=%7B%7D",
		"remote=%zz",
	}
	for _, rawQuery := range tests {
		t.Run(rawQuery, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1.51/build?"+rawQuery, nil)
			if !rejectsBuildOptions(request) {
				t.Fatal("option was admitted")
			}
		})
	}
	for _, rawQuery := range []string{"", "pull=0", "pull=false", "nocache=1", "target=final"} {
		t.Run("allow "+rawQuery, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1.51/build?"+rawQuery, nil)
			if rejectsBuildOptions(request) {
				t.Fatal("ordinary local option was rejected")
			}
		})
	}

	request := httptest.NewRequest(http.MethodPost, "/build", nil)
	request.Header.Set("X-Docker-Expose-Session-Uuid", "session-id")
	if !rejectsBuildOptions(request) {
		t.Fatal("session attachment header was admitted")
	}
}

func TestBuildAdmissionReplaysGzipContextAndChecksEverySource(t *testing.T) {
	rawContext := buildContextTar(t, true, []tarEntry{{name: "Dockerfile", body: "FROM loaded/base:1\nCOPY --from=loaded/tools:2 /tool /tool\n", kind: tar.TypeReg}})
	var inspected []string
	inspector := func(_ context.Context, sources []string) (map[string]imageInspection, error) {
		inspected = append([]string(nil), sources...)
		result := map[string]imageInspection{}
		for _, source := range sources {
			result[source] = imageInspection{present: true}
		}
		return result, nil
	}
	var replayed []byte
	next := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		replayed, _ = io.ReadAll(request.Body)
		response.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "/v1.51/build?t=result", bytes.NewReader(rawContext))
	request.Header.Set("Content-Encoding", "gzip")
	response := httptest.NewRecorder()
	buildAdmissionHandler(next, inspector).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	wantedSources := []string{"loaded/base:1", "loaded/tools:2"}
	if !reflect.DeepEqual(inspected, wantedSources) {
		t.Fatalf("inspected = %#v, want %#v", inspected, wantedSources)
	}
	if !bytes.Equal(replayed, rawContext) {
		t.Fatal("forwarded context was not byte-for-byte identical")
	}
}

func TestBuildAdmissionRejectsMissingOrOnBuildImageWithoutDisclosure(t *testing.T) {
	for name, inspection := range map[string]imageInspection{
		"missing": {},
		"onbuild": {present: true, hasOnBuild: true},
	} {
		t.Run(name, func(t *testing.T) {
			rawContext := buildContextTar(t, false, []tarEntry{{name: "Dockerfile", body: "ARG TOKEN\nFROM private.invalid/secret:1\n", kind: tar.TypeReg}})
			secret := "do-not-disclose"
			args, _ := url.QueryUnescape(`%7B%22TOKEN%22%3A%22do-not-disclose%22%7D`)
			request := httptest.NewRequest(http.MethodPost, "/build?buildargs="+url.QueryEscape(args), bytes.NewReader(rawContext))
			response := httptest.NewRecorder()
			inspector := func(_ context.Context, sources []string) (map[string]imageInspection, error) {
				return map[string]imageInspection{sources[0]: inspection}, nil
			}
			buildAdmissionHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("denied build was forwarded")
			}), inspector).ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			body := response.Body.String()
			for _, sensitive := range []string{"private.invalid", "secret:1", "TOKEN", secret} {
				if strings.Contains(body, sensitive) {
					t.Fatalf("response disclosed %q: %s", sensitive, body)
				}
			}
		})
	}
}

func TestBuildAdmissionRejectsUnsafeDockerfileEntries(t *testing.T) {
	tests := map[string][]tarEntry{
		"symlink":   {{name: "Dockerfile", kind: tar.TypeSymlink, link: "real"}, {name: "real", body: "FROM scratch\n", kind: tar.TypeReg}},
		"directory": {{name: "Dockerfile", kind: tar.TypeDir}},
		"duplicate": {{name: "Dockerfile", body: "FROM scratch\n", kind: tar.TypeReg}, {name: "./Dockerfile", body: "FROM scratch\n", kind: tar.TypeReg}},
		"traversal": {{name: "../Dockerfile", body: "FROM scratch\n", kind: tar.TypeReg}},
	}
	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			rawContext := buildContextTar(t, false, entries)
			request := httptest.NewRequest(http.MethodPost, "/build", bytes.NewReader(rawContext))
			response := httptest.NewRecorder()
			buildAdmissionHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("unsafe context was forwarded")
			}), func(context.Context, []string) (map[string]imageInspection, error) {
				return nil, nil
			}).ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestBuildAdmissionRejectsOversizedOrUnsupportedContext(t *testing.T) {
	t.Run("declared size", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/build", strings.NewReader("x"))
		request.ContentLength = maxBuildContextBytes + 1
		response := httptest.NewRecorder()
		buildAdmissionHandler(http.NotFoundHandler(), func(context.Context, []string) (map[string]imageInspection, error) { return nil, nil }).ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", response.Code)
		}
	})
	t.Run("encoding", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/build", strings.NewReader("x"))
		request.Header.Set("Content-Encoding", "zstd")
		response := httptest.NewRecorder()
		buildAdmissionHandler(http.NotFoundHandler(), func(context.Context, []string) (map[string]imageInspection, error) { return nil, nil }).ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", response.Code)
		}
	})
}

func TestReadDockerfileBoundsExpandedGzipContext(t *testing.T) {
	rawContext := buildContextTar(t, true, []tarEntry{
		{name: "Dockerfile", body: "FROM scratch\n", kind: tar.TypeReg},
		{name: "padding", body: strings.Repeat("x", 4096), kind: tar.TypeReg},
	})
	contextFile, err := os.CreateTemp(t.TempDir(), "context-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer contextFile.Close()
	if _, err := contextFile.Write(rawContext); err != nil {
		t.Fatal(err)
	}
	if _, err := readDockerfileWithLimit(contextFile, int64(len(rawContext)), "gzip", "Dockerfile", 2048); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("error = %v, want expanded context limit", err)
	}
}

func TestBackendImageInspectorReadsOnBuildWithoutPuttingSourceInErrors(t *testing.T) {
	tests := []struct {
		status  int
		body    string
		present bool
		onbuild bool
	}{
		{http.StatusOK, `{"Config":{"OnBuild":null}}`, true, false},
		{http.StatusOK, `{"Config":{"OnBuild":["COPY --from=remote/image /x /y"]}}`, true, true},
		{http.StatusNotFound, `{}`, false, false},
	}
	for _, tc := range tests {
		client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
		})}
		const source = "private.invalid/secret:1"
		got, err := backendImageInspector(client)(context.Background(), []string{source})
		if err != nil {
			t.Fatal(err)
		}
		if got[source] != (imageInspection{present: tc.present, hasOnBuild: tc.onbuild}) {
			t.Fatalf("inspection = %#v", got[source])
		}
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type tarEntry struct {
	name string
	body string
	kind byte
	link string
}

func buildContextTar(t *testing.T, compressed bool, entries []tarEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	var destination io.Writer = &output
	var gzipWriter *gzip.Writer
	if compressed {
		gzipWriter = gzip.NewWriter(&output)
		destination = gzipWriter
	}
	tarWriter := tar.NewWriter(destination)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Typeflag: entry.kind, Linkname: entry.link, Mode: 0o644}
		if entry.kind == tar.TypeReg || entry.kind == tar.TypeRegA {
			header.Size = int64(len(entry.body))
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size != 0 {
			if _, err := io.WriteString(tarWriter, entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if gzipWriter != nil {
		if err := gzipWriter.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return output.Bytes()
}
