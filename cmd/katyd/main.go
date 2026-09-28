package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"doppel.moe/katydid/internal/api"
	"doppel.moe/katydid/internal/importer"
	"doppel.moe/katydid/internal/library"
	"doppel.moe/katydid/internal/llm"
	"doppel.moe/katydid/internal/mb"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "katyd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	defaultSocket := os.Getenv("KATYDID_SOCKET")
	if defaultSocket == "" {
		defaultSocket = "/run/katyd/katyd.sock"
	}
	defaultLibrary := os.Getenv("KATYDID_LIBRARY")

	socket := flag.String("socket", defaultSocket, "unix socket path")
	libraryPath := flag.String("library", defaultLibrary, "music library root")
	flag.Parse()

	if *libraryPath == "" {
		return errors.New("no library root: set -library or KATYDID_LIBRARY")
	}
	info, err := os.Stat(*libraryPath)
	if err != nil {
		return fmt.Errorf("stat library %s: %w", *libraryPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("library %s is not a directory", *libraryPath)
	}

	index := library.NewIndex(*libraryPath)
	if err := index.Scan(); err != nil {
		return fmt.Errorf("initial scan: %w", err)
	}
	status := index.Status()
	fmt.Fprintf(os.Stderr, "katyd %s: library %s, %d albums (%d pending), scanned in %.2fs\n",
		status.Version, status.Library, status.Albums, status.Pending, status.ScanDuration)

	manager := newImporter(index)
	llmClient := llm.New(os.Getenv("KATYD_LLM_BASE_URL"), os.Getenv("KATYD_LLM_API_KEY"), os.Getenv("KATYD_LLM_MODEL"))
	if llmClient.Enabled() {
		fmt.Fprintf(os.Stderr, "katyd: llm candidate picking enabled via %s (%s)\n", os.Getenv("KATYD_LLM_BASE_URL"), os.Getenv("KATYD_LLM_MODEL"))
	}
	listener, err := listen(*socket)
	if err != nil {
		return err
	}
	defer os.Remove(*socket)

	server := &api.Server{Index: index, Import: manager, LLM: llmClient}
	httpServer := &http.Server{Handler: server.Handler()}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()

	select {
	case <-ctx.Done():
		return httpServer.Shutdown(context.Background())
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	}
}

func newImporter(index *library.Index) *importer.Manager {
	mbBase := os.Getenv("KATYDID_MB")
	cacheDir := os.Getenv("KATYDID_MB_CACHE")
	if cacheDir == "" {
		if user, err := os.UserCacheDir(); err == nil {
			cacheDir = filepath.Join(user, "katydid", "mb")
		}
	}
	noCache := os.Getenv("KATYDID_MB_NOCACHE") != ""
	return importer.New(index, mb.New(mbBase, cacheDir, noCache))
}

func listen(socket string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o775); err != nil {
		return nil, fmt.Errorf("create socket dir %s: %w", filepath.Dir(socket), err)
	}
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket %s: %w", socket, err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", socket, err)
	}
	if err := os.Chmod(socket, 0o660); err != nil {
		return nil, fmt.Errorf("chmod %s: %w", socket, err)
	}
	return listener, nil
}
