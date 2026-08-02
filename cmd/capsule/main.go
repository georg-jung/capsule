package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/georg-jung/capsule/internal/app"
	"github.com/georg-jung/capsule/internal/auth"
	"github.com/georg-jung/capsule/internal/store"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(os.Args[1:], logger); err != nil {
		logger.Error("capsule stopped", "error", err)
		os.Exit(1)
	}
}

func run(args []string, logger *slog.Logger) error {
	if len(args) != 0 {
		switch args[0] {
		case "reset-auth":
			return resetAuth(logger)
		case "healthcheck":
			return healthcheck()
		case "version":
			fmt.Println(version)
			return nil
		default:
			return fmt.Errorf("unknown command %q", args[0])
		}
	}
	return serve(logger)
}

func serve(logger *slog.Logger) error {
	config, err := app.ParseConfig(os.Getenv)
	if err != nil {
		return err
	}
	repository, err := store.Open(context.Background(), store.Config{
		DataDir:       config.DataDir,
		MaxUploadSize: config.MaxUploadSize,
	})
	if err != nil {
		return err
	}
	defer repository.Close()

	authenticator, err := auth.NewManager(config.Origin, config.RPID, repository, nil)
	if err != nil {
		return err
	}
	handler, err := app.NewServer(config, repository, authenticator)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              config.ListenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       90 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info("capsule listening", "address", config.ListenAddress, "origin", config.Origin, "version", version)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func resetAuth(logger *slog.Logger) error {
	dataDir := strings.TrimSpace(os.Getenv("CAPSULE_DATA_DIR"))
	if dataDir == "" {
		dataDir = "/data"
	}
	repository, err := store.Open(context.Background(), store.Config{DataDir: dataDir, SkipObjectPrune: true})
	if err != nil {
		return err
	}
	defer repository.Close()
	if err := repository.ResetAuth(context.Background()); err != nil {
		return err
	}
	logger.Info("authentication reset; uploaded files were preserved", "data_directory", dataDir)
	return nil
}

func healthcheck() error {
	url := strings.TrimSpace(os.Getenv("CAPSULE_HEALTHCHECK_URL"))
	if url == "" {
		url = "http://127.0.0.1:8080/healthz"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("health endpoint returned %s", response.Status)
	}
	return nil
}
