//go:build memtest

// Package memtest checks that cango pushes a large app to Neo4j inside a fixed memory limit (spec §7).
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
	"strconv"
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
	gib        = int64(1) << 30
)

func TestPushWithinMemoryLimit(t *testing.T) {
	apps := os.Getenv("MEMTEST_APPS")
	if apps == "" {
		t.Skip("set MEMTEST_APPS to run the memory-limit test")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
	limit := parseBytes(t, cmp.Or(os.Getenv("MEMTEST_MEMORY"), "16g"))
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

	for _, dir := range strings.Split(apps, ",") {
		dir, _ = filepath.Abs(strings.TrimSpace(dir))
		t.Run(filepath.Base(dir), func(t *testing.T) { runCango(t, ctx, nw, dir, limit) })
	}
}

// runCango starts the cango container on nw, waits for it to exit, and checks the exit state.
func runCango(t *testing.T, ctx context.Context, nw *testcontainers.DockerNetwork, dir string, limit int64) {
	_, thisFile, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..")
	// The app is mounted read-only at /src-app and copied to /app, because GOFLAGS=-mod=mod
	// lets go list rewrite go.mod or go.sum, and that must not touch the host checkout.
	script := "cp -a /src-app /app && " +
		"cango -i /app --emit neo4j -j \"$(nproc)\" -v -c /tmp/cango-cache; rc=$?; " +
		"echo MEMTEST_PEAK=$(cat /sys/fs/cgroup/memory.peak); exit $rc"
	c, err := testcontainers.Run(ctx, "",
		testcontainers.WithDockerfile(testcontainers.FromDockerfile{
			Context: repo, Dockerfile: "scripts/memtest/Dockerfile.cango",
			Repo: "cango-memtest", Tag: "latest", KeepImage: true,
		}),
		testcontainers.WithCmd(script),
		testcontainers.WithEnv(map[string]string{
			"NEO4J_URI": "bolt://neo4j:7687", "NEO4J_USERNAME": "neo4j", "NEO4J_PASSWORD": neo4jPass,
			"GOMODCACHE": "/gomod",
		}),
		network.WithNetwork(nil, nw),
		testcontainers.WithHostConfigModifier(func(h *container.HostConfig) {
			h.Memory, h.MemorySwap = limit, limit
			h.Mounts = append(h.Mounts,
				mount.Mount{Type: mount.TypeBind, Source: dir, Target: "/src-app", ReadOnly: true},
				mount.Mount{Type: mount.TypeVolume, Source: "cango-memtest-gomod", Target: "/gomod"})
		}),
		testcontainers.WithWaitStrategy(wait.ForExit().WithExitTimeout(5*time.Hour)),
	)
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	logs := containerLogs(t, ctx, c)
	peakText := "peak unknown (killed)"
	if peak := peakFrom(logs); peak > 0 {
		peakText = "peak " + strconv.FormatFloat(float64(peak)/float64(gib), 'f', 1, 64) + " GiB"
	}
	t.Logf("%s: exit %d, OOMKilled=%v, %s of %.1f GiB", filepath.Base(dir), st.ExitCode, st.OOMKilled,
		peakText, float64(limit)/float64(gib))
	// The kernel OOM killer usually kills only cango (Docker does not set memory.oom.group),
	// so the sh survives, prints the peak and exits 137. OOMKilled may stay false then:
	// exit 137 counts as an OOM kill on its own.
	if st.ExitCode != 0 || st.OOMKilled {
		t.Errorf("cango failed. Last log lines:\n%s", tail(logs, 40))
	}
}

// parseBytes reads a byte count with an optional k, m or g suffix (powers of 1024), as in "16g".
func parseBytes(t *testing.T, in string) int64 {
	t.Helper()
	s := strings.ToLower(strings.TrimSpace(in))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "k"):
		mult = 1 << 10
	case strings.HasSuffix(s, "m"):
		mult = 1 << 20
	case strings.HasSuffix(s, "g"):
		mult = gib
	}
	if mult != 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		t.Fatalf("bad memory size %q", in)
	}
	return n * mult
}

// containerLogs reads the container's stdout and stderr into one string.
func containerLogs(t *testing.T, ctx context.Context, c testcontainers.Container) string {
	t.Helper()
	rc, err := c.Logs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// peakFrom returns the byte count on the last MEMTEST_PEAK= line, or 0 when there is none.
func peakFrom(logs string) int64 {
	var peak int64
	for line := range strings.Lines(logs) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "MEMTEST_PEAK="); ok {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				peak = n
			}
		}
	}
	return peak
}

// tail returns the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
