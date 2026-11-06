// Command alcyona is the single-binary Alcyona assistant server:
// a web UI + agent harness over an external model, with all state
// kept locally in a single SQLite file.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/utopiafallen/alcyona-agent/internal/agent"
	"github.com/utopiafallen/alcyona-agent/internal/config"
	"github.com/utopiafallen/alcyona-agent/internal/modelclient"
	"github.com/utopiafallen/alcyona-agent/internal/server"
	"github.com/utopiafallen/alcyona-agent/internal/store"
)

func main() {
	var (
		addr    = flag.String("addr", envOr("ALCYONA_ADDR", ":8080"), "HTTP listen address")
		dataDir = flag.String("data", envOr("ALCYONA_DATA_DIR", "~/.alcyona"), "directory for local state")
	)
	flag.Parse()

	cfg := config.Load(*addr, *dataDir)
	log.SetPrefix("alcyona: ")
	log.Printf("starting (model %s @ %s, data %s)", cfg.ModelName, cfg.ModelBaseURL, cfg.DataDir)

	s, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer s.Close()

	model := modelclient.New(cfg.ModelBaseURL, cfg.ModelAPIKey, cfg.ModelName)
	ag := agent.New(model, s)
	srv := server.New(ag, s)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpSrv := &http.Server{Addr: cfg.Addr, Handler: srv}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", cfg.Addr)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Println("shutting down")
	case err := <-errCh:
		log.Fatalf("server: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("shutdown: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
