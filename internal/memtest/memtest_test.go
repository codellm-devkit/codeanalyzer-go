//go:build memtest

// The memory-limit container test (spec §7).
// Run: MEMTEST_APPS=<dir>[,<dir>...] go test -tags memtest ./internal/memtest -v -timeout 6h
//
// testcontainers-go does not read the docker CLI context. With Colima (or another non-default
// socket), set DOCKER_HOST to the socket and TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock.

package memtest

import (
	"cmp"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/testcontainers/testcontainers-go"
	tcneo4j "github.com/testcontainers/testcontainers-go/modules/neo4j"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	neo4jImage = "neo4j:5.26-community"
	neo4jPass  = "memtest-pass"
	// runTimeout bounds one cango run (analysis and push).
	runTimeout = 5 * time.Hour
)

func TestPushWithinMemoryLimit(t *testing.T) {
	apps := os.Getenv("MEMTEST_APPS")
	if apps == "" {
		t.Skip("set MEMTEST_APPS to run the memory-limit test")
	}
	limit, err := ParseBytes(cmp.Or(os.Getenv("MEMTEST_MEMORY"), "16g"))
	if err != nil {
		t.Fatal(err)
	}
	var dirs, bases []string
	for _, dir := range strings.Split(apps, ",") {
		dir, err := filepath.Abs(strings.TrimSpace(dir))
		if err != nil {
			t.Fatal(err)
		}
		dirs, bases = append(dirs, dir), append(bases, filepath.Base(dir))
	}
	// Each app gets its own can://<name> keyspace in the fresh Neo4j container, so every push is a
	// clean full push. Duplicate names fail here, before any container starts.
	names, err := AppNames(bases)
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()

	nw, err := network.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupNetwork(t, nw)

	db, err := tcneo4j.Run(ctx, neo4jImage,
		tcneo4j.WithAdminPassword(neo4jPass),
		tcneo4j.WithNeo4jSettings(map[string]string{
			"server.memory.heap.initial_size":   "8G",
			"server.memory.heap.max_size":       "8G",
			"server.memory.pagecache.size":      "2G",
			"dbms.memory.transaction.total.max": "5600m",
		}),
		network.WithNetwork([]string{"neo4j"}, nw),
		testcontainers.WithHostConfigModifier(func(h *container.HostConfig) {
			h.Memory, h.MemorySwap = 12*gib, 12*gib
		}),
	)
	testcontainers.CleanupContainer(t, db)
	if err != nil {
		t.Fatal(err)
	}

	for i, dir := range dirs {
		t.Run(names[i], func(t *testing.T) { runCango(t, ctx, nw, dir, names[i], limit) })
	}
}

// runCango starts the cango container on nw, waits for it to exit, and judges the run with Decide.
func runCango(t *testing.T, ctx context.Context, nw *testcontainers.DockerNetwork, dir, app string, limit int64) {
	_, thisFile, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..")
	// The app is mounted read-only at /src-app and copied to /app, because GOFLAGS=-mod=mod
	// lets go list rewrite go.mod or go.sum, and that must not touch the host checkout.
	// The app name comes in through MEMTEST_APP_NAME, never spliced into the script.
	script := "cp -a /src-app /app && " +
		"cango -i /app --app-name \"$MEMTEST_APP_NAME\" --emit neo4j -j \"$(nproc)\" -v -c /tmp/cango-cache; rc=$?; " +
		"echo " + peakMarker + "$(cat /sys/fs/cgroup/memory.peak); exit $rc"
	c, err := testcontainers.Run(ctx, "",
		testcontainers.WithDockerfile(testcontainers.FromDockerfile{
			Context: repo, Dockerfile: "scripts/memtest/Dockerfile.cango",
			Repo: "cango-memtest", Tag: "latest", KeepImage: true,
		}),
		testcontainers.WithCmd(script),
		testcontainers.WithEnv(map[string]string{
			"NEO4J_URI": "bolt://neo4j:7687", "NEO4J_USERNAME": "neo4j", "NEO4J_PASSWORD": neo4jPass,
			"GOMODCACHE": "/gomod", "MEMTEST_APP_NAME": app,
		}),
		network.WithNetwork(nil, nw),
		testcontainers.WithHostConfigModifier(func(h *container.HostConfig) {
			h.Memory, h.MemorySwap = limit, limit
			h.Mounts = append(h.Mounts,
				mount.Mount{Type: mount.TypeBind, Source: dir, Target: "/src-app", ReadOnly: true},
				mount.Mount{Type: mount.TypeVolume, Source: "cango-memtest-gomod", Target: "/gomod"})
		}),
		// ForExit returns nil as soon as the container is not running, whatever its exit code,
		// so a non-zero exit reaches Decide below, not this error path.
		// The wait is the readiness ("started") hook of Run. WithWaitStrategy would wrap it in
		// wait.ForAll with a 60 s deadline that caps the whole run, whatever the exit timeout:
		// the ForAll deadline must be the run bound too.
		testcontainers.WithWaitStrategyAndDeadline(runTimeout, wait.ForExit().WithExitTimeout(runTimeout)),
	)
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		if c != nil {
			t.Logf("last log lines:\n%s", Tail(containerLogs(t, ctx, c), 40))
		}
		t.Fatal(err)
	}
	st, err := c.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	logs := containerLogs(t, ctx, c)
	v := Decide(app, st.ExitCode, st.OOMKilled, logs, limit)
	t.Log(v.Line)
	if v.Failed {
		t.Errorf("cango failed. Last log lines:\n%s", Tail(logs, 40))
	}
}

// containerLogs reads the container's stdout and stderr into one string ("" if they cannot be read).
func containerLogs(t *testing.T, ctx context.Context, c testcontainers.Container) string {
	t.Helper()
	rc, err := c.Logs(ctx)
	if err != nil {
		t.Logf("read logs: %v", err)
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Logf("read logs: %v", err)
	}
	return string(b)
}
