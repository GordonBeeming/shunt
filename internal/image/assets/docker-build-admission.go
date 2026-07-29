package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/distribution/reference"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	"github.com/moby/buildkit/util/gitutil"
)

const (
	maxBuildContextBytes         = int64(8 << 30)
	maxExpandedBuildContextBytes = int64(8 << 30)
	maxDockerfileBytes           = int64(1 << 20)
	maxBuildArgsBytes            = 1 << 20
	buildDenialMessage           = "shunt offline policy rejects this Docker build"
	buildContextMessage          = "shunt offline policy rejects this Docker build context"
)

type imageInspection struct {
	present    bool
	hasOnBuild bool
}

type imageInspector func(context.Context, []string) (map[string]imageInspection, error)

// buildAdmissionHandler admits a Docker build only when every Dockerfile image
// source already exists in the private daemon. It replays the original build
// context byte-for-byte after inspecting a bounded, private temporary copy.
func buildAdmissionHandler(next http.Handler, inspect imageInspector) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !isDockerEndpoint(request.Method, request.URL, "/build") {
			next.ServeHTTP(response, request)
			return
		}
		if inspect == nil || rejectsBuildOptions(request) {
			writeDockerError(response, http.StatusForbidden, buildDenialMessage)
			return
		}

		contextFile, dockerfile, err := spoolBuildContext(request)
		if err != nil {
			writeDockerError(response, http.StatusBadRequest, buildContextMessage)
			return
		}
		defer func() {
			_ = contextFile.Close()
			_ = os.Remove(contextFile.Name())
		}()

		buildArgs, err := parseBuildArgs(request.URL.Query().Get("buildargs"))
		if err != nil {
			writeDockerError(response, http.StatusForbidden, buildDenialMessage)
			return
		}
		sources, err := dockerfileImageSources(dockerfile, buildArgs)
		if err != nil {
			writeDockerError(response, http.StatusForbidden, buildDenialMessage)
			return
		}
		inspections, err := inspect(request.Context(), sources)
		if err != nil {
			writeDockerError(response, http.StatusBadGateway, "shunt offline policy could not verify this Docker build")
			return
		}
		for _, source := range sources {
			inspection := inspections[source]
			if !inspection.present || inspection.hasOnBuild {
				writeDockerError(response, http.StatusForbidden, buildDenialMessage)
				return
			}
		}

		if _, err := contextFile.Seek(0, io.SeekStart); err != nil {
			writeDockerError(response, http.StatusInternalServerError, buildContextMessage)
			return
		}
		request.Body = contextFile
		next.ServeHTTP(response, request)
	})
}

func rejectsBuildOptions(request *http.Request) bool {
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return true
	}
	// Remote contexts, session attachments, registry cache imports/exports, and
	// non-local outputs all let BuildKit obtain sources which are not present in
	// the submitted tar or the private daemon's image store.
	for _, key := range []string{
		"remote", "session", "cachefrom", "cache-from", "cacheto", "cache-to",
		"cacheimports", "cache-imports", "cacheexports", "cache-exports", "outputs",
	} {
		values, present := query[key]
		if !present {
			continue
		}
		if len(values) != 1 || values[0] != "" {
			return true
		}
	}
	// The admission parser and daemon must not be allowed to disagree about
	// which Dockerfile, build arguments, or pull policy a repeated field means.
	for _, key := range []string{"dockerfile", "buildargs", "pull"} {
		if len(query[key]) > 1 {
			return true
		}
	}
	for _, value := range query["pull"] {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", "0", "false":
		case "1", "true":
			return true
		default:
			return true
		}
	}
	for name, values := range request.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-docker-expose-session-") {
			for _, value := range values {
				if value != "" {
					return true
				}
			}
		}
	}
	return false
}

func isDockerEndpoint(method string, requestURL *url.URL, endpoint string) bool {
	if method != http.MethodPost {
		return false
	}
	return dockerAPIPath(requestURL) == endpoint
}

func dockerAPIPath(requestURL *url.URL) string {
	requestPath := path.Clean("/" + strings.TrimPrefix(requestURL.Path, "/"))
	unversioned := dockerVersionPrefix.ReplaceAllString(requestPath, "")
	return strings.TrimSuffix(unversioned, "/")
}

func spoolBuildContext(request *http.Request) (_ *os.File, dockerfile []byte, resultErr error) {
	if request.Body == nil || request.ContentLength > maxBuildContextBytes {
		return nil, nil, errors.New("invalid build context")
	}
	contentEncodings := request.Header.Values("Content-Encoding")
	if len(contentEncodings) > 1 {
		return nil, nil, errors.New("unsupported build context encoding")
	}
	contentEncoding := strings.ToLower(strings.TrimSpace(request.Header.Get("Content-Encoding")))
	if contentEncoding != "" && contentEncoding != "gzip" && contentEncoding != "x-gzip" {
		return nil, nil, errors.New("unsupported build context encoding")
	}

	contextFile, err := os.CreateTemp("", "shunt-build-context-*")
	if err != nil {
		return nil, nil, errors.New("create build context spool")
	}
	defer func() {
		if resultErr != nil {
			_ = contextFile.Close()
			_ = os.Remove(contextFile.Name())
		}
	}()
	if err := contextFile.Chmod(0o600); err != nil {
		return nil, nil, errors.New("secure build context spool")
	}
	written, err := io.CopyN(contextFile, request.Body, maxBuildContextBytes+1)
	_ = request.Body.Close()
	if written > maxBuildContextBytes {
		return nil, nil, errors.New("build context too large")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("read build context")
	}
	request.ContentLength = written

	target, ok := cleanContextPath(request.URL.Query().Get("dockerfile"))
	if !ok {
		return nil, nil, errors.New("invalid Dockerfile path")
	}
	if target == "" {
		target = "Dockerfile"
	}
	dockerfile, err = readDockerfile(contextFile, written, contentEncoding, target)
	if err != nil {
		return nil, nil, err
	}
	return contextFile, dockerfile, nil
}

func readDockerfile(contextFile *os.File, size int64, contentEncoding, target string) ([]byte, error) {
	return readDockerfileWithLimit(contextFile, size, contentEncoding, target, maxExpandedBuildContextBytes)
}

func readDockerfileWithLimit(contextFile *os.File, size int64, contentEncoding, target string, expandedLimit int64) ([]byte, error) {
	if expandedLimit < 0 {
		return nil, errors.New("invalid expanded build context limit")
	}
	reader := bufio.NewReader(io.NewSectionReader(contextFile, 0, size))
	magic, _ := reader.Peek(2)
	isGzip := len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b
	if (contentEncoding == "gzip" || contentEncoding == "x-gzip") && !isGzip {
		return nil, errors.New("invalid gzip build context")
	}
	var tarInput io.Reader = reader
	if isGzip {
		gzipReader, err := gzip.NewReader(reader)
		if err != nil {
			return nil, errors.New("invalid gzip build context")
		}
		defer gzipReader.Close()
		tarInput = gzipReader
	}

	expanded := &io.LimitedReader{R: tarInput, N: expandedLimit + 1}
	tarReader := tar.NewReader(expanded)
	var dockerfile []byte
	found := false
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if expanded.N == 0 {
				return nil, errors.New("expanded build context too large")
			}
			return nil, errors.New("invalid build context archive")
		}
		entry, safe := cleanContextPath(header.Name)
		if !safe || entry != target {
			continue
		}
		if found || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) || header.Size < 0 || header.Size > maxDockerfileBytes {
			return nil, errors.New("unsafe Dockerfile archive entry")
		}
		found = true
		dockerfile = make([]byte, header.Size)
		if _, err := io.ReadFull(tarReader, dockerfile); err != nil {
			if expanded.N == 0 {
				return nil, errors.New("expanded build context too large")
			}
			return nil, errors.New("truncated Dockerfile archive entry")
		}
	}
	if expanded.N == 0 {
		return nil, errors.New("expanded build context too large")
	}
	if !found {
		return nil, errors.New("Dockerfile missing from build context")
	}
	return dockerfile, nil
}

func cleanContextPath(value string) (string, bool) {
	if value == "" {
		return "", true
	}
	if strings.ContainsRune(value, 0) || path.IsAbs(value) || strings.Contains(value, "\\") {
		return "", false
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", false
		}
	}
	cleaned := strings.TrimPrefix(path.Clean(value), "./")
	if cleaned == "." || cleaned == "" {
		return "", false
	}
	return cleaned, true
}

func parseBuildArgs(encoded string) (map[string]*string, error) {
	if encoded == "" {
		return map[string]*string{}, nil
	}
	if len(encoded) > maxBuildArgsBytes {
		return nil, errors.New("build arguments too large")
	}
	args := map[string]*string{}
	if err := json.Unmarshal([]byte(encoded), &args); err != nil || len(args) > 1024 {
		return nil, errors.New("invalid build arguments")
	}
	for key := range args {
		if key == "" || strings.ContainsAny(key, "=\x00\r\n") {
			return nil, errors.New("invalid build argument name")
		}
	}
	return args, nil
}

func dockerfileImageSources(dockerfile []byte, buildArgs map[string]*string) ([]string, error) {
	parsed, err := parser.Parse(strings.NewReader(string(dockerfile)))
	if err != nil {
		return nil, errors.New("invalid Dockerfile")
	}
	stages, metaArgs, err := instructions.Parse(parsed.AST, nil)
	if err != nil || len(stages) == 0 {
		return nil, errors.New("unsupported Dockerfile")
	}
	lex := shell.NewLex(parsed.EscapeToken)
	global := map[string]string{}
	if err := applyArgCommands(global, metaArgs, buildArgs, nil, lex); err != nil {
		return nil, err
	}

	sources := map[string]struct{}{}
	if frontend, _, _, ok := parser.DetectSyntax(dockerfile); ok {
		if err := addExternalSource(sources, frontend, nil, -1); err != nil {
			return nil, err
		}
	}
	aliases := map[string]int{}
	for stageIndex := range stages {
		stage := &stages[stageIndex]
		base, err := expandSource(stage.BaseName, global, lex)
		if err != nil {
			return nil, err
		}
		if err := addExternalSource(sources, base, aliases, stageIndex); err != nil {
			return nil, err
		}

		stageEnv := map[string]string{}
		for _, command := range stage.Commands {
			switch command := command.(type) {
			case *instructions.ArgCommand:
				if err := applyArgCommands(stageEnv, []instructions.ArgCommand{*command}, buildArgs, global, lex); err != nil {
					return nil, err
				}
			case *instructions.EnvCommand:
				if err := applyEnvCommand(stageEnv, command, lex); err != nil {
					return nil, err
				}
			case *instructions.CopyCommand:
				if command.From != "" {
					from, err := expandSource(command.From, stageEnv, lex)
					if err != nil {
						return nil, err
					}
					if err := addExternalSource(sources, from, aliases, stageIndex); err != nil {
						return nil, err
					}
				}
			case *instructions.RunCommand:
				for _, mount := range instructions.GetMounts(command) {
					if mount.From == "" {
						continue
					}
					from, err := expandSource(mount.From, stageEnv, lex)
					if err != nil {
						return nil, err
					}
					if err := addExternalSource(sources, from, aliases, stageIndex); err != nil {
						return nil, err
					}
				}
			case *instructions.AddCommand:
				for _, source := range command.SourcePaths {
					expanded, err := expandSource(source, stageEnv, lex)
					if err != nil || !isLocalAddSource(expanded) {
						return nil, errors.New("external ADD source rejected")
					}
				}
			case *instructions.OnbuildCommand:
				return nil, errors.New("ONBUILD source analysis is unsupported")
			}
		}
		if stage.Name != "" {
			aliases[strings.ToLower(stage.Name)] = stageIndex
		}
	}

	result := make([]string, 0, len(sources))
	for source := range sources {
		result = append(result, source)
	}
	sort.Strings(result)
	return result, nil
}

func applyArgCommands(env map[string]string, commands []instructions.ArgCommand, overrides map[string]*string, inherited map[string]string, lex *shell.Lex) error {
	for _, command := range commands {
		for _, arg := range command.Args {
			value, hasValue := "", false
			if override, ok := overrides[arg.Key]; ok && override != nil {
				value, hasValue = *override, true
			} else if arg.Value != nil {
				expanded, err := expandSource(*arg.Value, env, lex)
				if err != nil {
					return err
				}
				value, hasValue = expanded, true
			} else if inherited != nil {
				value, hasValue = inherited[arg.Key]
			}
			if hasValue {
				env[arg.Key] = value
			}
		}
	}
	return nil
}

func applyEnvCommand(env map[string]string, command *instructions.EnvCommand, lex *shell.Lex) error {
	updates := map[string]string{}
	for _, pair := range command.Env {
		value, err := expandSource(pair.Value, env, lex)
		if err != nil {
			return err
		}
		updates[pair.Key] = value
	}
	for key, value := range updates {
		env[key] = value
	}
	return nil
}

func expandSource(value string, env map[string]string, lex *shell.Lex) (string, error) {
	envs := make([]string, 0, len(env))
	for key, value := range env {
		envs = append(envs, key+"="+value)
	}
	result, err := lex.ProcessWordWithMatches(value, shell.EnvsFromSlice(envs))
	if err != nil || len(result.Unmatched) != 0 || result.Result == "" || strings.ContainsAny(result.Result, "\x00\r\n") {
		return "", errors.New("unresolved Dockerfile source")
	}
	return result.Result, nil
}

func addExternalSource(sources map[string]struct{}, source string, aliases map[string]int, currentStage int) error {
	if source == "scratch" {
		return nil
	}
	if aliases != nil {
		if _, ok := aliases[strings.ToLower(source)]; ok {
			return nil
		}
		if index, err := strconv.Atoi(source); err == nil && index >= 0 && index < currentStage {
			return nil
		}
	}
	if strings.TrimSpace(source) != source || strings.ContainsAny(source, " \t") {
		return errors.New("invalid Dockerfile image source")
	}
	if _, err := reference.ParseAnyReference(source); err != nil {
		return errors.New("invalid Dockerfile image source")
	}
	sources[source] = struct{}{}
	return nil
}

func isLocalAddSource(source string) bool {
	lower := strings.ToLower(source)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || gitutil.IsGitTransport(source) {
		return false
	}
	return true
}

func backendImageInspector(client *http.Client) imageInspector {
	return func(ctx context.Context, sources []string) (map[string]imageInspection, error) {
		inspections := make(map[string]imageInspection, len(sources))
		for _, source := range sources {
			endpoint := "http://dockerd/images/" + url.PathEscape(source) + "/json"
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if err != nil {
				return nil, errors.New("prepare image inspection")
			}
			result, err := client.Do(request)
			if err != nil {
				return nil, errors.New("inspect image source")
			}
			switch result.StatusCode {
			case http.StatusOK:
				var body struct {
					Config *struct {
						OnBuild []string `json:"OnBuild"`
					} `json:"Config"`
				}
				decoder := json.NewDecoder(io.LimitReader(result.Body, 2<<20))
				if err := decoder.Decode(&body); err != nil {
					_ = result.Body.Close()
					return nil, errors.New("decode image inspection")
				}
				if body.Config == nil {
					_ = result.Body.Close()
					return nil, errors.New("image inspection lacks configuration")
				}
				inspections[source] = imageInspection{present: true, hasOnBuild: len(body.Config.OnBuild) != 0}
			case http.StatusNotFound:
				inspections[source] = imageInspection{}
			default:
				_ = result.Body.Close()
				return nil, fmt.Errorf("image inspection status %d", result.StatusCode)
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(result.Body, 64<<10))
			_ = result.Body.Close()
		}
		return inspections, nil
	}
}
