#!/bin/sh
# shunt siding entrypoint: bring up the in-guest Docker daemon, then run whatever
# command shunt passed (normally the Aspire AppHost). dockerd is what Aspire's
# DCP orchestrator talks to to start the app's dependency containers in this guest.
set -e

dockerd >/var/log/dockerd.log 2>&1 &

# Wait for the Docker socket so the AppHost doesn't race the daemon.
i=0
while [ "$i" -lt 60 ]; do
    if docker info >/dev/null 2>&1; then
        break
    fi
    i=$((i + 1))
    sleep 1
done
if ! docker info >/dev/null 2>&1; then
    echo "shunt-entrypoint: dockerd did not become ready in 60s" >&2
    tail -n 40 /var/log/dockerd.log >&2 || true
    exit 1
fi

# Generate the ASP.NET Core dev certificate so projects with HTTPS endpoints can
# bind Kestrel (otherwise they crash: "developer certificate could not be found").
dotnet dev-certs https >/dev/null 2>&1 || true

echo "shunt-entrypoint: dockerd ready; launching command: $*"
exec "$@"
