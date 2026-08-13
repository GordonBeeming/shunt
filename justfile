# shunt build/install — one recipe per channel. The build-time Channel ldflag is
# the single source of truth; everything else (paths, ports, labels, container
# prefix) derives from it, so release/beta/nightly/dev channels install and run
# side by side.

mod := "github.com/gordonbeeming/shunt/internal/config"
bindir := env_var_or_default("SHUNT_BINDIR", env_var("HOME") + "/.local/bin")

_default:
    @just --list

# Build the release channel binary (command: shunt) into dist/.
build-release:
    go build -ldflags "-X {{mod}}.Channel=release" -o dist/shunt .

# Build the beta channel binary (command: shunt-beta) into dist/.
build-beta:
    go build -ldflags "-X {{mod}}.Channel=beta" -o dist/shunt-beta .

# Build the nightly channel binary (command: shunt-nightly) into dist/.
build-nightly:
    go build -ldflags "-X {{mod}}.Channel=nightly" -o dist/shunt-nightly .

# Build the dev channel binary (command: shunt-dev) into dist/.
build-dev:
    go build -ldflags "-X {{mod}}.Channel=dev" -o dist/shunt-dev .

# Build all channels.
build-all: build-release build-beta build-nightly build-dev

install-release: build-release
    install -d {{bindir}} && install -m 0755 dist/shunt {{bindir}}/shunt

install-beta: build-beta
    install -d {{bindir}} && install -m 0755 dist/shunt-beta {{bindir}}/shunt-beta

install-nightly: build-nightly
    install -d {{bindir}} && install -m 0755 dist/shunt-nightly {{bindir}}/shunt-nightly

install-dev: build-dev
    install -d {{bindir}} && install -m 0755 dist/shunt-dev {{bindir}}/shunt-dev

test:
    go test ./...

tidy:
    go mod tidy
