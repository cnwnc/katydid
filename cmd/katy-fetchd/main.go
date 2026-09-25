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
	"strconv"
	"syscall"
	"time"

	"doppel.moe/katydid/internal/cli"
	"doppel.moe/katydid/internal/fetch"
	"doppel.moe/katydid/internal/slskd"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "katy-fetchd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	defaultSocket := os.Getenv("KATYFETCHD_SOCKET")
	if defaultSocket == "" {
		defaultSocket = filepath.Join(os.TempDir(), "katy-fetchd.sock")
	}
	defaultState := os.Getenv("KATYFETCHD_STATE")
	if defaultState == "" {
		if home, err := os.UserHomeDir(); err == nil {
			defaultState = filepath.Join(home, ".local", "state", "katydid", "fetchd.json")
		}
	}
	defaultKatyd := os.Getenv("KATYFETCHD_KATYD")
	if defaultKatyd == "" {
		defaultKatyd = os.Getenv("KATYDID_SOCKET")
	}
	if defaultKatyd == "" {
		defaultKatyd = filepath.Join(os.TempDir(), "katyd.sock")
	}
	defaultSlskd := os.Getenv("KATYFETCHD_SLSKD")
	if defaultSlskd == "" {
		defaultSlskd = "http://127.0.0.1:5030"
	}
	defaultDownloads := os.Getenv("KATYFETCHD_DOWNLOADS")
	defaultKey := os.Getenv("KATYFETCHD_API_KEY")
	if defaultKey == "" {
		defaultKey = os.Getenv("SLSKD_API_KEY")
	}
	defaultPoll := 10
	if raw := os.Getenv("KATYFETCHD_POLL"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			defaultPoll = parsed
		}
	}

	socket := flag.String("socket", defaultSocket, "unix socket path")
	state := flag.String("state", defaultState, "want queue state file")
	katyd := flag.String("katyd", defaultKatyd, "katyd unix socket path")
	slskdBase := flag.String("slskd", defaultSlskd, "slskd http base url")
	apiKey := flag.String("api-key", defaultKey, "slskd api key (X-API-Key)")
	downloads := flag.String("downloads", defaultDownloads, "slskd downloads directory")
	poll := flag.Int("poll", defaultPoll, "seconds between ticks")
	flag.Parse()

	if *apiKey == "" {
		return errors.New("no slskd api key: set -api-key or KATYFETCHD_API_KEY")
	}
	if *downloads == "" {
		return errors.New("no downloads directory: set -downloads or KATYFETCHD_DOWNLOADS")
	}
	info, err := os.Stat(*downloads)
	if err != nil {
		return fmt.Errorf("stat downloads %s: %w", *downloads, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("downloads %s is not a directory", *downloads)
	}
	if *state == "" {
		return errors.New("no state file: set -state or KATYFETCHD_STATE")
	}

	store, err := fetch.OpenStore(*state)
	if err != nil {
		return err
	}
	orchestrator := fetch.New(store, fetch.Config{
		Slskd:        slskd.New(*slskdBase, *apiKey),
		Katyd:        cli.Dial(*katyd),
		DownloadsDir: *downloads,
	})

	listener, err := listen(*socket)
	if err != nil {
		return err
	}
	defer os.Remove(*socket)

	server := &fetch.Server{Orchestrator: orchestrator}
	httpServer := &http.Server{Handler: server.Handler()}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ticker := time.NewTicker(time.Duration(*poll) * time.Second)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				orchestrator.Tick(ctx)
			}
		}
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()

	fmt.Fprintf(os.Stderr, "katy-fetchd: socket %s, slskd %s, katyd %s, downloads %s\n",
		*socket, *slskdBase, *katyd, *downloads)

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

func listen(socket string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o775); err != nil {
		return nil, fmt.Errorf("create socket dir %s: %w", filepath.Dir(socket), err)
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
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
