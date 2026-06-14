package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/kumagaias/tailagent/internal/model"
	"github.com/kumagaias/tailagent/internal/server"
	storesqlite "github.com/kumagaias/tailagent/internal/storage/sqlite"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println("tailagent", version)
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var dataDir string
	var port int
	var openBrowser bool
	flag.StringVar(&dataDir, "data-dir", filepath.Join(home, ".config", "tailagent"), "directory containing the SQLite database")
	flag.IntVar(&port, "port", 8787, "localhost port")
	flag.BoolVar(&openBrowser, "open", false, "open the local UI in the default browser")
	flag.Parse()
	if port < 1 || port > 65535 {
		fmt.Fprintln(os.Stderr, "port must be between 1 and 65535")
		os.Exit(2)
	}
	dbPath := filepath.Join(dataDir, "tailagent.db")
	store, err := storesqlite.Open(dbPath)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	defaults := model.Settings{WorkspaceRoot: home, DatabasePath: dbPath, ApprovalTimeoutSecs: 300, DefaultShell: os.Getenv("SHELL"), MaxConcurrentAgents: 2, TraceRetentionDays: 30}
	settings, err := store.Settings(context.Background(), defaults)
	if err != nil {
		slog.Error("load settings", "error", err)
		os.Exit(1)
	}
	app := server.New(store, settings)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.RunMaintenance(ctx)
	httpServer := &http.Server{Addr: server.ListenAddress(port), Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	url := "http://" + httpServer.Addr
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	if openBrowser {
		go func() {
			time.Sleep(250 * time.Millisecond)
			if err := launchBrowser(url); err != nil {
				slog.Warn("open browser", "error", err)
			}
		}()
	}
	slog.Info("tailagent started", "url", url, "database", dbPath)
	if err = httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("serve", "error", err)
		os.Exit(1)
	}
}

func launchBrowser(url string) error {
	var command string
	switch runtime.GOOS {
	case "darwin":
		command = "open"
	case "linux":
		command = "xdg-open"
	default:
		return fmt.Errorf("automatic browser opening is not supported on %s", runtime.GOOS)
	}
	return exec.Command(command, url).Start()
}
