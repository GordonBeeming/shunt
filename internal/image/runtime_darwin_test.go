package image

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestAppleContainerGuestDockerAdmission(t *testing.T) {
	if os.Getenv("SHUNT_CONTAINER_INTEGRATION") != "1" {
		t.Skip("set SHUNT_CONTAINER_INTEGRATION=1 to build and run the guest base image")
	}
	if err := exec.Command("container", "system", "status").Run(); err != nil {
		t.Skipf("Apple container runtime is unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	baseTag := Tag()
	baseExisted := Exists(ctx, baseTag)
	if err := EnsureBuilt(ctx, false); err != nil {
		t.Fatalf("build content-versioned base image: %v", err)
	}
	if !baseExisted {
		t.Cleanup(func() {
			output, err := exec.Command("container", "image", "delete", baseTag).CombinedOutput()
			if err != nil {
				t.Errorf("delete disposable base image %s: %v\n%s", baseTag, err, output)
			}
		})
	}

	guest := fmt.Sprintf("shunt-admission-integration-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		var lastOutput []byte
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			lastOutput, lastErr = exec.Command("container", "delete", "--force", guest).CombinedOutput()
			if lastErr == nil {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Errorf("delete disposable guest %s: %v\n%s", guest, lastErr, lastOutput)
	})
	runHost(t, ctx, "container", "run", "--detach", "--cap-add", "ALL", "--name", guest, baseTag, "sleep", "600")

	deadline := time.Now().Add(90 * time.Second)
	for {
		command := exec.CommandContext(ctx, "container", "exec", guest, "sh", "-c", "docker info >/dev/null 2>&1")
		if command.Run() == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := exec.CommandContext(ctx, "container", "logs", guest).CombinedOutput()
			diagnostics, _ := exec.CommandContext(ctx, "container", "exec", guest, "sh", "-c", "cat /var/log/dockerd.log /var/log/shunt-docker-api-admission.log 2>/dev/null || true; ps -ef").CombinedOutput()
			t.Fatalf("guest Docker admission socket did not become ready in 90 seconds\ncontainer logs:\n%s\nguest diagnostics:\n%s", logs, diagnostics)
		}
		time.Sleep(time.Second)
	}

	// Kill the backend once, then race two repair commands. The guest-local
	// lock must let exactly one invocation repair dockerd while the other waits
	// and observes the repaired daemon as healthy.
	repairRace := `set -eu
backend_pid=$(cat /var/run/docker.pid)
kill "$backend_pid"
i=0
while kill -0 "$backend_pid" 2>/dev/null && [ "$i" -lt 20 ]; do
    i=$((i + 1))
    sleep 1
done
rm -f /run/shunt/dockerd-offline.ready /tmp/shunt-repair-1.log /tmp/shunt-repair-2.log
/usr/local/bin/shunt-dockerd-offline >/tmp/shunt-repair-1.log 2>&1 &
repair_one=$!
/usr/local/bin/shunt-dockerd-offline >/tmp/shunt-repair-2.log 2>&1 &
repair_two=$!
wait "$repair_one"
wait "$repair_two"
repair_count=$(grep -h -c 'repairing dockerd' /tmp/shunt-repair-1.log /tmp/shunt-repair-2.log | awk '{ total += $1 } END { print total + 0 }')
test "$repair_count" = 1
docker info >/dev/null
`
	runHost(t, ctx, "container", "exec", guest, "sh", "-c", repairRace)

	script := `set -eux
proof_tag=shunt-admission-proof:latest
proof_container=shunt-admission-proof
proof_volume=shunt-admission-volume
proof_network=shunt-admission-network
build_dir=/tmp/shunt-admission-build
derived_dir=/tmp/shunt-admission-derived
missing_dir=/tmp/shunt-admission-missing
rm -rf "$build_dir"
mkdir -p "$build_dir"
cp /usr/local/bin/shunt-docker-api-admission "$build_dir/proof"
printf '%s\n' 'FROM scratch' 'COPY proof /proof' 'ENTRYPOINT ["/proof", "--listen", "/tmp/proof.sock", "--backend", "/tmp/missing.sock"]' > "$build_dir/Containerfile"
docker build -f "$build_dir/Containerfile" -t "$proof_tag" "$build_dir"
docker image inspect "$proof_tag" >/dev/null
test "$DOCKER_BUILDKIT" = 0
grpc_status=$(curl --silent --show-error --unix-socket /var/run/docker.sock --output /tmp/shunt-grpc-denial.json --write-out '%{http_code}' --request POST http://localhost/v1.51/grpc)
test "$grpc_status" = 403
grep -F 'prebakeBuilds' /tmp/shunt-grpc-denial.json >/dev/null
if DOCKER_BUILDKIT=1 docker build -f "$build_dir/Containerfile" -t shunt-admission-buildx-bypass:latest "$build_dir" >/tmp/shunt-buildx-bypass.log 2>&1; then
    echo 'buildx tunnel unexpectedly succeeded' >&2
    exit 1
fi
test -s /tmp/shunt-buildx-bypass.log
rm -rf "$derived_dir"
mkdir -p "$derived_dir"
printf '%s\n' "FROM $proof_tag" 'LABEL shunt.preloaded=true' > "$derived_dir/Containerfile"
docker build -f "$derived_dir/Containerfile" -t shunt-admission-derived:latest "$derived_dir"
docker image save -o /tmp/shunt-admission-proof.tar "$proof_tag"
docker image rm "$proof_tag" >/dev/null
docker image load -i /tmp/shunt-admission-proof.tar >/dev/null
docker run --detach --name "$proof_container" "$proof_tag" >/dev/null
test "$(docker inspect --format '{{.State.Running}}' "$proof_container")" = true
docker rm --force "$proof_container" >/dev/null
docker volume create "$proof_volume" >/dev/null
docker volume inspect "$proof_volume" >/dev/null
docker volume rm "$proof_volume" >/dev/null
docker network create "$proof_network" >/dev/null
docker network inspect "$proof_network" >/dev/null
docker network rm "$proof_network" >/dev/null
rm -f /tmp/shunt-registry-hits
node -e 'const fs=require("fs"),http=require("http"); http.createServer((req,res)=>{fs.appendFileSync("/tmp/shunt-registry-hits",req.method+" "+req.url+"\n"); res.writeHead(404); res.end();}).listen(5000,"127.0.0.1")' &
registry_pid=$!
trap 'kill "$registry_pid" 2>/dev/null || true' EXIT
sleep 1
assert_registry_denied() {
    denial_name=$1
    denial_method=$2
    denial_url=$3
    denial_output=/tmp/shunt-registry-denial-$denial_name.json
    if test "$#" = 4; then
        denial_status=$(curl --silent --show-error --unix-socket /var/run/docker.sock --output "$denial_output" --write-out '%{http_code}' --request "$denial_method" --header 'Content-Type: application/json' --data "$4" "$denial_url")
    else
        denial_status=$(curl --silent --show-error --unix-socket /var/run/docker.sock --output "$denial_output" --write-out '%{http_code}' --request "$denial_method" "$denial_url")
    fi
    test "$denial_status" = 403
    grep -F 'shunt offline policy rejects Docker registry access' "$denial_output" >/dev/null
}
assert_registry_denied search GET 'http://localhost/v1.51/images/search?term=localhost%3A5000%2Fnever'
assert_registry_denied distribution GET 'http://localhost/v1.51/distribution/localhost:5000%2Fnever:latest/json'
assert_registry_denied plugin-privileges GET 'http://localhost/v1.51/plugins/privileges?remote=localhost%3A5000%2Fnever%3Alatest'
assert_registry_denied plugin-pull POST 'http://localhost/v1.51/plugins/pull?remote=localhost%3A5000%2Fnever%3Alatest' '[]'
assert_registry_denied plugin-push POST 'http://localhost/v1.51/plugins/example/push'
assert_registry_denied plugin-upgrade POST 'http://localhost/v1.51/plugins/example/upgrade?remote=localhost%3A5000%2Fnever%3Alatest' '[]'
assert_registry_denied auth POST 'http://localhost/v1.51/auth' '{"serveraddress":"http://localhost:5000"}'
assert_registry_denied service-create POST 'http://localhost/v1.51/services/create' '{"TaskTemplate":{"ContainerSpec":{"Image":"localhost:5000/never:latest"}}}'
assert_registry_denied service-update POST 'http://localhost/v1.51/services/example/update?version=1' '{"TaskTemplate":{"ContainerSpec":{"Image":"localhost:5000/never:latest"}}}'
docker tag "$proof_tag" localhost:5000/proof:latest
if push_output=$(docker push localhost:5000/proof:latest 2>&1); then
    echo 'loopback push unexpectedly succeeded' >&2
    exit 1
fi
printf '%s\n' "$push_output" | grep -F 'shunt offline policy rejects Docker registry access' >/dev/null
docker image rm localhost:5000/proof:latest >/dev/null
rm -rf "$missing_dir"
mkdir -p "$missing_dir"
printf '%s\n' 'FROM localhost:5000/never:latest' > "$missing_dir/Containerfile"
if docker build -f "$missing_dir/Containerfile" -t shunt-admission-missing:latest "$missing_dir" >/tmp/shunt-missing-build.log 2>&1; then
    echo 'build from missing loopback base unexpectedly succeeded' >&2
    exit 1
fi
kill "$registry_pid" 2>/dev/null || true
wait "$registry_pid" 2>/dev/null || true
trap - EXIT
if test -s /tmp/shunt-registry-hits; then
    echo 'missing-base build contacted loopback registry' >&2
    cat /tmp/shunt-missing-build.log >&2
    cat /tmp/shunt-registry-hits >&2
    cat /var/log/shunt-docker-api-admission.log >&2 2>/dev/null || true
    exit 1
fi
grep -F 'shunt offline policy rejects this Docker build' /tmp/shunt-missing-build.log >/dev/null
if pull_output=$(docker pull localhost:5000/never:latest 2>&1); then
    echo 'loopback pull unexpectedly succeeded' >&2
    exit 1
fi
printf '%s\n' "$pull_output" | grep -F 'shunt offline policy rejects Docker image pulls' >/dev/null
`
	runHost(t, ctx, "container", "exec", guest, "sh", "-c", script)
}

func runHost(t *testing.T, ctx context.Context, name string, args ...string) {
	t.Helper()
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, output)
	}
}
