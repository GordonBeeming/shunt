package caddy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/proc"
)

// l4Module is the layer4 plugin that gives Caddy raw TCP proxying (for DB/TCP
// front-door routes alongside HTTP).
const l4Module = "github.com/mholt/caddy-l4"

// Build produces this channel's Caddy binary (Caddy + caddy-l4) via xcaddy,
// writing it to the channel's global dir. It's a no-op if the binary already
// exists unless force is set.
func Build(ctx context.Context, force bool) (string, error) {
	binPath, err := config.CaddyBinaryPath()
	if err != nil {
		return "", err
	}
	if !force {
		if _, statErr := os.Stat(binPath); statErr == nil {
			return binPath, nil
		}
	}
	if !proc.Look("xcaddy") {
		return "", fmt.Errorf("xcaddy not found on PATH; install it with " +
			"`go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest`")
	}
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		return "", fmt.Errorf("create caddy dir: %w", err)
	}
	if err := proc.RunPassthrough(ctx, "xcaddy", "build", "--output", binPath, "--with", l4Module); err != nil {
		return "", fmt.Errorf("xcaddy build: %w", err)
	}
	return binPath, nil
}
