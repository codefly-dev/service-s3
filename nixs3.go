package main

// nixs3.go — Docker-free s3 (minio) runtime (mirrors postgres' nixpg.go, neo4j's
// nixneo4j.go, redis' nixredis.go).
//
// The s3 service agent runs minio in a container by default
// (NewDockerHeadlessEnvironment, `minio server /data`). On hosts without Docker,
// the same agent runs minio NATIVELY from a nix-provisioned binary: the codefly
// NixEnvironment materializes `minio` from the embedded flake, and this file
// launches `minio server <dataDir> --address 127.0.0.1:<port>` with the
// configured root credentials, then waits for the live health endpoint. Both
// runtimes serve the S3 API on the same assigned port, so WaitForReady is
// unchanged.

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
)

//go:embed nix/flake.nix
var minioFlakeNix string

//go:embed nix/flake.lock
var minioFlakeLock string

// nixMinio runs a native single-node minio server off a nix-provisioned binary.
type nixMinio struct {
	env          *runners.NixEnvironment
	flakeDir     string
	dataDir      string
	port         uint16
	rootUser     string
	rootPassword string
	out          io.Writer
	proc         runners.Proc
	// serverCtx is the context minio runs under. It MUST outlive Init: starting
	// minio under the Init RPC's ctx kills it the instant Init returns. Cancelled
	// only by Stop.
	serverCtx    context.Context
	serverCancel context.CancelFunc
	// binDir is the absolute nix store bin dir holding the minio binary.
	binDir string
}

// newNixMinio materializes the embedded flake under baseDir/nix and prepares a
// native minio rooted at baseDir/minio. baseDir is the agent's local service
// dir, so bucket data persists across restarts like the Docker volume.
func newNixMinio(ctx context.Context, baseDir string, port uint16, rootUser, rootPassword string, out io.Writer) (*nixMinio, error) {
	flakeDir := filepath.Join(baseDir, "nix")
	if err := os.MkdirAll(flakeDir, 0o755); err != nil {
		return nil, fmt.Errorf("create nix flake dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(flakeDir, "flake.nix"), []byte(minioFlakeNix), 0o644); err != nil {
		return nil, fmt.Errorf("write flake.nix: %w", err)
	}
	if err := os.WriteFile(filepath.Join(flakeDir, "flake.lock"), []byte(minioFlakeLock), 0o644); err != nil {
		return nil, fmt.Errorf("write flake.lock: %w", err)
	}
	env, err := runners.NewNixEnvironment(ctx, flakeDir)
	if err != nil {
		return nil, fmt.Errorf("nix environment (is nix installed?): %w", err)
	}
	env.WithCacheDir(filepath.Join(baseDir, ".nix-cache"))
	return &nixMinio{
		env:          env,
		flakeDir:     flakeDir,
		dataDir:      filepath.Join(baseDir, "minio"),
		port:         port,
		rootUser:     rootUser,
		rootPassword: rootPassword,
		out:          out,
	}, nil
}

// Init materializes the nix env, locates minio, launches it on the assigned
// port, and waits for the live health endpoint.
func (n *nixMinio) Init(ctx context.Context) error {
	if err := n.env.Init(ctx); err != nil {
		return fmt.Errorf("materialize nix minio env: %w", err)
	}
	if err := n.resolveStore(); err != nil {
		return err
	}
	if err := os.MkdirAll(n.dataDir, 0o755); err != nil {
		return fmt.Errorf("create minio data dir: %w", err)
	}
	if err := n.startServer(ctx); err != nil {
		return err
	}
	return n.waitReady(ctx)
}

// resolveStore locates the nix-store minio binary by absolute path so we run the
// nix-built minio even if a system minio shadows PATH.
func (n *nixMinio) resolveStore() error {
	matches, err := filepath.Glob("/nix/store/*-minio-*/bin/minio")
	if err != nil {
		return fmt.Errorf("glob nix minio: %w", err)
	}
	if len(matches) == 0 {
		return fmt.Errorf("no nix minio with bin/minio found in /nix/store (materialization may have failed)")
	}
	n.binDir = filepath.Dir(matches[0])
	return nil
}

// startServer launches `minio server <dataDir> --address 127.0.0.1:<port>` with
// the root credentials passed via env (as the Docker path does). --console-address
// is omitted: codefly exposes only the S3 API port.
func (n *nixMinio) startServer(ctx context.Context) error {
	proc, err := n.env.NewProcess(filepath.Join(n.binDir, "minio"),
		"server", n.dataDir,
		"--address", fmt.Sprintf("127.0.0.1:%d", n.port),
	)
	if err != nil {
		return err
	}
	proc.WithEnvironmentVariables(ctx,
		resources.Env("MINIO_ROOT_USER", n.rootUser),
		resources.Env("MINIO_ROOT_PASSWORD", n.rootPassword),
	)
	if n.out != nil {
		proc.WithOutput(n.out)
	}
	n.serverCtx, n.serverCancel = context.WithCancel(context.Background())
	if err := proc.Start(n.serverCtx); err != nil {
		n.serverCancel()
		return fmt.Errorf("start minio: %w", err)
	}
	n.proc = proc
	return nil
}

// waitReady polls minio's live health endpoint until it returns 200.
func (n *nixMinio) waitReady(ctx context.Context) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/minio/health/live", n.port)
	client := &http.Client{Timeout: 1 * time.Second}
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("health status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("minio did not become ready on %s: %w", url, lastErr)
}

// Stop terminates the minio server process.
func (n *nixMinio) Stop(ctx context.Context) error {
	if n.serverCancel != nil {
		n.serverCancel()
	}
	if n.proc == nil {
		return nil
	}
	return n.proc.Stop(ctx)
}
