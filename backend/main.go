package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	prefix := envOr("GATEWAY_PREFIX", "/app/diskbench")
	socketPath := os.Getenv("SOCKET_PATH")
	port := envInt("PORT", 5050)
	localDev := os.Getenv("LOCAL_DEV") == "1"
	mockDev := localDev && os.Getenv("MOCK_DEV") == "1"

	exeDir, err := os.Executable()
	baseDir := "."
	if err == nil {
		baseDir = filepath.Dir(exeDir)
	}
	dataDir := resolveRuntimePath(os.Getenv("DATA_DIR"), filepath.Join(baseDir, "data"))
	publicDir := resolveRuntimePath(os.Getenv("FRONTEND_DIST"), filepath.Join(baseDir, "public"))

	mockDisks := defaultMockDisks()
	var resolveFn func(string) (*Disk, error)
	var resolveSpaceFn func(string) (*StorageSpace, error)
	if mockDev {
		mockSpaces := defaultMockSpaces(mockDisks)
		resolveFn = func(p string) (*Disk, error) {
			for i := range mockDisks {
				if mockDisks[i].Path == p {
					return &mockDisks[i], nil
				}
			}
			return nil, nil
		}
		resolveSpaceFn = func(p string) (*StorageSpace, error) {
			for i := range mockSpaces {
				if mockSpaces[i].Path == p {
					return &mockSpaces[i], nil
				}
			}
			return nil, nil
		}
	}
	jobs := NewJobManager(dataDir, JobManagerOptions{Mock: mockDev, ResolveDisk: resolveFn, ResolveSpace: resolveSpaceFn})
	handler := newAPIServer(apiServerOptions{
		Jobs:      jobs,
		LocalDev:  localDev,
		MockDev:   mockDev,
		MockDisks: mockDisks,
		Prefix:    prefix,
		PublicDir: publicDir,
	})

	server := &http.Server{Handler: handler}

	shutdown := func() {
		jobs.shutdown()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		if socketPath != "" {
			_ = os.Remove(socketPath)
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		shutdown()
		os.Exit(0)
	}()

	if socketPath != "" {
		_ = os.Remove(socketPath)
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Disk Bench listen failed: %v\n", err)
			os.Exit(1)
		}
		_ = os.Chmod(socketPath, 0o660)
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "Disk Bench serve failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	addr := fmt.Sprintf("%s:%d", envOr("LISTEN_HOST", "127.0.0.1"), port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Disk Bench listen failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Disk Bench listening at http://%s%s\n", addr, prefix)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "Disk Bench serve failed: %v\n", err)
		os.Exit(1)
	}
}
