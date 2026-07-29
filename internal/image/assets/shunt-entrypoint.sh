#!/bin/sh
# shunt siding entrypoint: bring up the in-guest Docker daemon, then run whatever
# command shunt passed (normally the Aspire AppHost). dockerd is what Aspire's
# DCP orchestrator talks to to start the app's dependency containers in this guest.
set -eu

# Docker honours these variables in the daemon process for registry traffic.
# The proxy endpoint is deliberately closed, so pulls fail immediately inside
# the guest without reaching a registry. We intentionally do not exempt
# localhost: a registry can listen there too. Docker's Unix control socket and
# networking inside ordinary application containers do not use this environment.
offline_policy_version=4
offline_proxy=http://127.0.0.1:1
offline_ready_marker=/run/shunt/dockerd-offline.ready
dockerd_pid_file=/var/run/docker.pid
dockerd_log=/var/log/dockerd.log
admission_program=/usr/local/bin/shunt-docker-api-admission
admission_pid_file=/run/shunt/docker-api-admission.pid
admission_log=/var/log/shunt-docker-api-admission.log
admission_socket=/var/run/docker.sock
backend_dir=/run/shunt/dockerd
backend_socket=$backend_dir/docker.sock
dockerd_lock_file=/run/shunt/dockerd-offline.lock

print_policy_env() {
    printf '%s\n' \
        "HTTP_PROXY=$offline_proxy" \
        "HTTPS_PROXY=$offline_proxy" \
        "http_proxy=$offline_proxy" \
        "https_proxy=$offline_proxy" \
        "NO_PROXY=" \
        "no_proxy=" \
        "SHUNT_DOCKERD_OFFLINE_POLICY=$offline_policy_version"
}

dockerd_pid() {
    [ -r "$dockerd_pid_file" ] || return 1
    pid=$(sed -n '1p' "$dockerd_pid_file")
    case "$pid" in
        ''|*[!0-9]*) return 1 ;;
    esac
    process_is_running "$pid" || return 1
    printf '%s\n' "$pid"
}

admission_pid() {
    [ -r "$admission_pid_file" ] || return 1
    admission_process=$(sed -n '1p' "$admission_pid_file")
    case "$admission_process" in
        ''|*[!0-9]*) return 1 ;;
    esac
    process_is_running "$admission_process" || return 1
    printf '%s\n' "$admission_process"
}

process_is_running() {
    process_pid=$1
    kill -0 "$process_pid" 2>/dev/null || return 1
    if [ -r "/proc/$process_pid/stat" ]; then
        process_stat=$(sed -n '1p' "/proc/$process_pid/stat")
        case "$process_stat" in
            *') Z '*) return 1 ;;
        esac
    fi
    return 0
}

daemon_has_offline_policy() {
    daemon_pid=$1
    environ=/proc/$daemon_pid/environ
    [ -r "$environ" ] || return 1
    env_lines=$(tr '\000' '\n' < "$environ")
    printf '%s\n' "$env_lines" | grep -Fqx "HTTP_PROXY=$offline_proxy" || return 1
    printf '%s\n' "$env_lines" | grep -Fqx "HTTPS_PROXY=$offline_proxy" || return 1
    printf '%s\n' "$env_lines" | grep -Fqx "NO_PROXY=" || return 1
    printf '%s\n' "$env_lines" | grep -Fqx "SHUNT_DOCKERD_OFFLINE_POLICY=$offline_policy_version"
}

write_ready_marker() {
    marker_pid=$1
    mkdir -p "${offline_ready_marker%/*}"
    marker_tmp=$offline_ready_marker.tmp.$$
    umask 077
    {
        printf 'version=%s\n' "$offline_policy_version"
        printf 'pid=%s\n' "$marker_pid"
        printf 'proxy=%s\n' "$offline_proxy"
        printf 'admission=%s\n' "$admission_socket"
        printf 'backend=%s\n' "$backend_socket"
    } > "$marker_tmp"
    mv -f "$marker_tmp" "$offline_ready_marker"
}

stop_admission() {
    if admission_process=$(admission_pid); then
        if ! kill "$admission_process" 2>/dev/null; then
            echo "shunt-dockerd-offline: failed to TERM admission PID $admission_process" >&2
        fi
        i=0
        while process_is_running "$admission_process" && [ "$i" -lt 20 ]; do
            i=$((i + 1))
            sleep 1
        done
        if process_is_running "$admission_process"; then
            if ! kill -9 "$admission_process" 2>/dev/null; then
                echo "shunt-dockerd-offline: failed to KILL admission PID $admission_process" >&2
            fi
        fi
    fi
    rm -f "$admission_pid_file" "$admission_socket"
}

stop_dockerd() {
    rm -f "$offline_ready_marker"
    stop_admission
    if pid=$(dockerd_pid); then
        if ! kill "$pid" 2>/dev/null; then
            echo "shunt-dockerd-offline: failed to TERM dockerd PID $pid" >&2
        fi
        i=0
        while process_is_running "$pid" && [ "$i" -lt 20 ]; do
            i=$((i + 1))
            sleep 1
        done
        if process_is_running "$pid"; then
            if ! kill -9 "$pid" 2>/dev/null; then
                echo "shunt-dockerd-offline: failed to KILL dockerd PID $pid" >&2
            fi
        fi
    fi

    # A failed dockerd can leave its child containerd behind even after its PID
    # is gone. This guest is dedicated to one siding, so cleaning that child is
    # both safe and necessary before a deterministic restart.
    if [ -r /var/run/docker/containerd/containerd.pid ]; then
        containerd_pid=$(sed -n '1p' /var/run/docker/containerd/containerd.pid)
        case "$containerd_pid" in
            ''|*[!0-9]*) ;;
            *) if ! kill "$containerd_pid" 2>/dev/null; then
                   echo "shunt-dockerd-offline: failed to TERM containerd PID $containerd_pid" >&2
               fi ;;
        esac
    fi
    rm -f "$dockerd_pid_file" /var/run/docker/containerd/containerd.pid "$backend_socket"
}

backend_docker_ready() {
    DOCKER_HOST="unix://$backend_socket" docker info >/dev/null 2>&1
}

admitted_docker_ready() {
    admission_pid >/dev/null \
        && DOCKER_HOST="unix://$admission_socket" docker info >/dev/null 2>&1
}

start_admission() {
    : > "$admission_log"
    "$admission_program" --listen "$admission_socket" --backend "$backend_socket" >>"$admission_log" 2>&1 &
    admission_process=$!
    umask 077
    printf '%s\n' "$admission_process" > "$admission_pid_file"
}

ensure_offline_dockerd() {
    repair_reason="missing dockerd PID"
    prior_pid=""
    if pid=$(dockerd_pid); then
        prior_pid=$pid
        if ! backend_docker_ready; then
            repair_reason="backend Docker health check failed"
        elif ! admitted_docker_ready; then
            repair_reason="admission proxy health check failed"
        elif ! daemon_has_offline_policy "$pid"; then
            repair_reason="offline policy mismatch"
        else
            write_ready_marker "$pid"
            return 0
        fi
    fi

    echo "shunt-dockerd-offline: repairing dockerd; reason=$repair_reason prior_pid=${prior_pid:-none}" >&2

    # Preserve the log that explains why the previous daemon was rejected.
    # A dedicated guest may repair itself, but losing the triggering evidence
    # makes the next failure needlessly opaque.
    if [ -s "$dockerd_log" ]; then
        stamp=$(date -u +%Y%m%dT%H%M%SZ)
        rotated="$dockerd_log.$stamp"
        if mv "$dockerd_log" "$rotated"; then
            echo "shunt-dockerd-offline: rotating prior dockerd log to $rotated" >&2
        else
            echo "shunt-dockerd-offline: failed to rotate prior dockerd log" >&2
        fi
    fi
    stop_dockerd
    mkdir -p "${dockerd_log%/*}" "$backend_dir"
    chmod 0700 "$backend_dir"
    : > "$dockerd_log"

    HTTP_PROXY=$offline_proxy \
    HTTPS_PROXY=$offline_proxy \
    http_proxy=$offline_proxy \
    https_proxy=$offline_proxy \
    NO_PROXY='' \
    no_proxy='' \
    SHUNT_DOCKERD_OFFLINE_POLICY=$offline_policy_version \
        dockerd --host="unix://$backend_socket" >>"$dockerd_log" 2>&1 &

    i=0
    while [ "$i" -lt 60 ]; do
        if pid=$(dockerd_pid) \
            && backend_docker_ready \
            && daemon_has_offline_policy "$pid"; then
            start_admission
            break
        fi
        i=$((i + 1))
        sleep 1
    done

    i=0
    while [ "$i" -lt 30 ]; do
        if admitted_docker_ready; then
            write_ready_marker "$pid"
            return 0
        fi
        i=$((i + 1))
        sleep 1
    done

    echo "shunt-dockerd-offline: dockerd admission did not become ready with pull denial" >&2
    tail -n 40 "$dockerd_log" >&2 || true
    tail -n 40 "$admission_log" >&2 || true
    stop_dockerd
    return 1
}

if [ "${1:-}" = "--print-policy-env" ]; then
    print_policy_env
    exit 0
fi
if [ "${1:-}" = "--print-policy-contract" ]; then
    printf 'version=%s\nmarker=%s\n' "$offline_policy_version" "$offline_ready_marker"
    exit 0
fi

mkdir -p "${dockerd_lock_file%/*}"
exec 9>"$dockerd_lock_file"
if ! flock -w 120 9; then
    echo "shunt-dockerd-offline: timed out waiting for startup lock $dockerd_lock_file" >&2
    exit 1
fi
ensure_offline_dockerd
flock -u 9
exec 9>&-

# The same asset is installed under a second name so lifecycle recovery can
# idempotently enforce the exact startup policy without running entrypoint work.
if [ "${0##*/}" = "shunt-dockerd-offline" ]; then
    exit 0
fi

# Generate the ASP.NET Core dev certificate so projects with HTTPS endpoints can
# bind Kestrel (otherwise they crash: "developer certificate could not be found").
dotnet dev-certs https >/dev/null 2>&1 || true
# Trust it in the guest's system CA bundle so .NET HttpClient (health checks,
# service-to-service https between Aspire projects) accepts it. Linux has no
# per-user trust store like macOS, and `dotnet dev-certs --trust` only half-works
# here, so export the cert and add it to ca-certificates — the bundle OpenSSL
# (and thus HttpClient) actually reads. Without this, https services come up
# "Running (Unhealthy)" with "The SSL connection could not be established".
if dotnet dev-certs https --export-path /usr/local/share/ca-certificates/aspnetcore-dev.crt --format PEM --no-password >/dev/null 2>&1; then
  update-ca-certificates >/dev/null 2>&1 || true

  # Some non-Chromium clients read trust from the NSS database
  # (sql:$HOME/.pki/nssdb via certutil), not the system CA bundle just updated
  # above, so import the same cert there too. This does NOT make Chromium
  # accept the dev cert: it is a self-signed leaf (not a CA), and Chromium's builtin verifier
  # requires CA:true on a trust anchor regardless of NSS trust flags — the
  # baked playwright-cli config's `--allow-insecure-localhost` launch arg is
  # what actually waives cert errors for Chromium, scoped to localhost
  # origins (see the Containerfile). `certutil -N` on an already-initialized
  # database prompts for a password it can never get in a guest with no TTY
  # and hangs retrying forever, so only run it the first time; re-adding the
  # same nickname on later boots is a harmless no-op.
  if [ ! -f "$HOME/.pki/nssdb/cert9.db" ]; then
    mkdir -p "$HOME/.pki/nssdb"
    certutil -d "sql:$HOME/.pki/nssdb" -N --empty-password </dev/null >/dev/null 2>&1 \
      || echo "shunt-entrypoint: warning: could not initialize NSS certificate database" >&2
  fi
  certutil -d "sql:$HOME/.pki/nssdb" -A -t "C,," -n aspnetcore-dev -i /usr/local/share/ca-certificates/aspnetcore-dev.crt </dev/null >/dev/null 2>&1 \
    || echo "shunt-entrypoint: warning: could not trust dev cert in NSS certificate database" >&2
fi

echo "shunt-entrypoint: offline dockerd ready; launching command: $*"
exec "$@"
