// Package caddy wraps the per-channel Caddy server: building it (xcaddy with the
// layer4 module), generating its bootstrap config, and driving its admin API to
// add routes and repoint upstreams live.
package caddy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gordonbeeming/shunt/internal/config"
)

// Admin is a thin client over this channel's Caddy admin API (127.0.0.1:<port>).
type Admin struct {
	base string
	http *http.Client
}

// NewAdmin returns an admin client pointed at the current channel's admin port.
func NewAdmin() *Admin {
	return &Admin{
		base: config.AdminBaseURL(),
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// Ping reports whether the admin API is up (GET /config/ returns 2xx).
func (a *Admin) Ping(ctx context.Context) error {
	return a.do(ctx, http.MethodGet, "/config/", nil)
}

// Load replaces the entire Caddy config (POST /load).
func (a *Admin) Load(ctx context.Context, configJSON []byte) error {
	return a.do(ctx, http.MethodPost, "/load", configJSON)
}

// GetConfig returns the full live config (GET /config/).
func (a *Admin) GetConfig(ctx context.Context) ([]byte, error) {
	return a.read(ctx, http.MethodGet, "/config/", nil)
}

// GetID returns the config object tagged with the given @id (GET /id/<id>).
func (a *Admin) GetID(ctx context.Context, id string) ([]byte, error) {
	return a.read(ctx, http.MethodGet, "/id/"+id, nil)
}

// Patch updates the object at an admin path in place (PATCH). path is relative
// to the admin root, e.g. "/id/app_x_http_frontend/upstreams/0".
func (a *Admin) Patch(ctx context.Context, path string, body []byte) error {
	return a.do(ctx, http.MethodPatch, path, body)
}

// Post appends/creates at an admin path (POST), e.g. appending a route to a
// server's routes array.
func (a *Admin) Post(ctx context.Context, path string, body []byte) error {
	return a.do(ctx, http.MethodPost, path, body)
}

// Put creates a new value at an admin path (PUT), e.g. a new named server. Caddy
// treats PUT as create; use it when registering a route's server.
func (a *Admin) Put(ctx context.Context, path string, body []byte) error {
	return a.do(ctx, http.MethodPut, path, body)
}

// Delete removes the config at path. Callers ignore the error when clearing a
// route that may not exist yet.
func (a *Admin) Delete(ctx context.Context, path string) error {
	return a.do(ctx, http.MethodDelete, path, nil)
}

func (a *Admin) do(ctx context.Context, method, path string, body []byte) error {
	_, err := a.read(ctx, method, path, body)
	return err
}

func (a *Admin) read(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("build %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: caddy admin returned %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(data))
	}
	return data, nil
}
