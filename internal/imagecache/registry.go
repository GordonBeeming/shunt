package imagecache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const registryOperationTimeout = 2 * time.Minute

const maxCredentialFormBytes = 1 << 20

func pullImage(ctx context.Context, ref name.Reference, platform v1.Platform) (v1.Image, error) {
	keychain, err := inlineKeychainFromEnv()
	if err != nil {
		return nil, err
	}
	transport := &domainBoundTransport{
		base:         remote.DefaultTransport,
		registryHost: ref.Context().RegistryStr(),
	}
	return remote.Image(
		ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(keychain),
		remote.WithPlatform(platform),
		remote.WithTransport(transport),
	)
}

type dockerAuthConfig struct {
	Auths       map[string]authn.AuthConfig `json:"auths"`
	CredsStore  string                      `json:"credsStore,omitempty"`
	CredHelpers map[string]string           `json:"credHelpers,omitempty"`
}

type inlineKeychain struct {
	auths map[string]authn.AuthConfig
}

func inlineKeychainFromEnv() (authn.Keychain, error) {
	raw := strings.TrimSpace(os.Getenv("DOCKER_AUTH_CONFIG"))
	if raw == "" {
		return inlineKeychain{auths: map[string]authn.AuthConfig{}}, nil
	}
	var config dockerAuthConfig
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return nil, fmt.Errorf("parse DOCKER_AUTH_CONFIG: %w", redact(err))
	}
	auths := make(map[string]authn.AuthConfig, len(config.Auths))
	for key, value := range config.Auths {
		auths[normalizeAuthKey(key)] = value
	}
	// credsStore and credHelpers are intentionally ignored. Shunt never
	// executes Docker Desktop, OrbStack, or another external helper.
	return inlineKeychain{auths: auths}, nil
}

func (keychain inlineKeychain) Resolve(resource authn.Resource) (authn.Authenticator, error) {
	keys := []string{resource.String(), resource.RegistryStr()}
	if resource.RegistryStr() == name.DefaultRegistry {
		keys = append(keys, authn.DefaultAuthKey, "docker.io", "registry-1.docker.io")
	}
	for _, key := range keys {
		if config, ok := keychain.auths[normalizeAuthKey(key)]; ok && !emptyAuthConfig(config) {
			return authn.FromConfig(config), nil
		}
	}
	return authn.Anonymous, nil
}

func emptyAuthConfig(config authn.AuthConfig) bool {
	return config.Username == "" && config.Password == "" && config.Auth == "" &&
		config.IdentityToken == "" && config.RegistryToken == ""
}

func normalizeAuthKey(key string) string {
	key = strings.TrimSpace(strings.ToLower(key))
	key = strings.TrimPrefix(key, "https://")
	key = strings.TrimPrefix(key, "http://")
	key = strings.TrimSuffix(key, "/v1/")
	key = strings.TrimSuffix(key, "/v1")
	return strings.TrimSuffix(key, "/")
}

type domainBoundTransport struct {
	base         http.RoundTripper
	registryHost string
}

func (transport *domainBoundTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := transport.base
	if base == nil {
		base = http.DefaultTransport
	}
	requestHost := normalizeAuthority(request.URL.Host, request.URL.Scheme)
	credentialed, err := credentialBearing(request)
	if err != nil {
		return nil, fmt.Errorf("inspect registry request credentials: %w", redact(err))
	}
	if credentialed && !transport.allowedCredentialHost(requestHost) {
		return nil, fmt.Errorf("refusing to send registry credentials to host %q for registry %q", requestHost, normalizeAuthority(transport.registryHost, "https"))
	}
	return base.RoundTrip(request)
}

func (transport *domainBoundTransport) allowedCredentialHost(host string) bool {
	registry := normalizeAuthority(transport.registryHost, "https")
	if host == registry {
		return true
	}
	allowed := builtInRegistryAuthHosts[registry]
	for _, candidate := range allowed {
		if host == candidate {
			return true
		}
	}
	return false
}

var builtInRegistryAuthHosts = map[string][]string{
	"docker.io":            {"index.docker.io", "registry-1.docker.io", "auth.docker.io"},
	"index.docker.io":      {"docker.io", "registry-1.docker.io", "auth.docker.io"},
	"registry-1.docker.io": {"docker.io", "index.docker.io", "auth.docker.io"},
}

func credentialBearing(request *http.Request) (bool, error) {
	if request.Header.Get("Authorization") != "" || request.Header.Get("Proxy-Authorization") != "" || request.Header.Get("Cookie") != "" {
		return true, nil
	}
	if request.URL.User != nil {
		if _, set := request.URL.User.Password(); set || request.URL.User.Username() != "" {
			return true, nil
		}
	}
	for key := range request.URL.Query() {
		if credentialParameter(key) {
			return true, nil
		}
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil && request.Header.Get("Content-Type") != "" {
		return false, fmt.Errorf("parse Content-Type: %w", err)
	}
	if mediaType != "application/x-www-form-urlencoded" || request.Body == nil {
		return false, nil
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxCredentialFormBytes+1))
	if err != nil {
		return false, fmt.Errorf("read form body: %w", err)
	}
	if closeErr := request.Body.Close(); closeErr != nil {
		return false, fmt.Errorf("close form body: %w", closeErr)
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	if len(body) > maxCredentialFormBytes {
		return false, fmt.Errorf("form body exceeds %d bytes", maxCredentialFormBytes)
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return false, fmt.Errorf("parse form body: %w", err)
	}
	for key := range values {
		if credentialParameter(key) {
			return true, nil
		}
	}
	return false, nil
}

func credentialParameter(key string) bool {
	key = strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(strings.TrimSpace(key)))
	switch key {
	case "password", "passwd", "token", "accesstoken", "refreshtoken", "identitytoken", "registrytoken", "secret", "clientsecret", "authorization", "auth", "code":
		return true
	default:
		return false
	}
}

func normalizeAuthority(host, scheme string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if parsed, err := url.Parse(host); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	if strings.HasSuffix(host, ".") {
		host = strings.TrimSuffix(host, ".")
	}
	hostname, port, err := net.SplitHostPort(host)
	if err == nil {
		hostname = strings.TrimSuffix(strings.ToLower(hostname), ".")
		if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
			return hostname
		}
		return net.JoinHostPort(hostname, port)
	}
	return host
}

// redact keeps structured credentials, authorization values, bearer tokens,
// URL userinfo, and query secrets out of errors surfaced to users.
func redact(err error) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	s = urlUserInfo.ReplaceAllString(s, `${1}…@`)
	s = jsonSecret.ReplaceAllString(s, `${1}"…"`)
	s = namedSecret.ReplaceAllString(s, `${1}${2}…`)
	s = bearerSecret.ReplaceAllString(s, `${1} …`)
	return errors.New(s)
}

var (
	urlUserInfo  = regexp.MustCompile(`(?i)(https?://)[^/@\s]+:[^/@\s]+@`)
	jsonSecret   = regexp.MustCompile(`(?i)("(?:password|passwd|token|access_token|refresh_token|identitytoken|registrytoken|authorization|secret|auth)"\s*:\s*)"[^"]*"`)
	namedSecret  = regexp.MustCompile(`(?i)\b(password|passwd|token|access_token|refresh_token|identitytoken|registrytoken|authorization|secret|auth)(\s*[=:]\s*)(?:bearer\s+|basic\s+)?[^\s,;&}]+`)
	bearerSecret = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[a-z0-9._~+/=-]+`)
)
