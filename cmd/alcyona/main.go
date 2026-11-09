// Command alcyona is the single-binary Alcyona assistant server: a web UI +
// agent harness over an external model, with all state kept locally (per-user
// SQLite). On first run (no server.toml) it serves the token-gated setup
// wizard; after setup it serves the normal app — switching in-process, no
// restart.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/utopiafallen/alcyona-agent/internal/auth"
	"github.com/utopiafallen/alcyona-agent/internal/config"
	"github.com/utopiafallen/alcyona-agent/internal/server"
	"github.com/utopiafallen/alcyona-agent/internal/setup"
	"github.com/utopiafallen/alcyona-agent/internal/store"
)

func main() {
	var (
		port = flag.Int("port", 0, "required: HTTP listen port (no default)")
		bind = flag.String("bind", "0.0.0.0", "bind host (default 0.0.0.0)")
		data = flag.String("data", "", "data dir (default: the directory containing this binary)")
	)
	flag.Parse()

	if *port <= 0 {
		fmt.Fprintln(os.Stderr, "alcyona: -port is required (no default)")
		flag.Usage()
		os.Exit(2)
	}

	dataDir := *data
	var err error
	if dataDir == "" {
		dataDir, err = config.DefaultDataDir()
		if err != nil {
			log.Fatalf("alcyona: cannot resolve binary dir: %v", err)
		}
	} else {
		dataDir, err = config.CleanPath(dataDir)
		if err != nil {
			log.Fatalf("alcyona: bad -data: %v", err)
		}
	}

	srvStore, err := store.OpenServer(dataDir)
	if err != nil {
		log.Fatalf("alcyona: open server store: %v", err)
	}
	defer srvStore.Close()
	prov := auth.NewInternalProvider(srvStore, time.Duration(config.DefaultSessionTTLSeconds)*time.Second)

	router := &server.Router{}

	if config.Exists(dataDir) {
		// Normal mode: server.toml already exists.
		app, err := server.NewNormal(dataDir, srvStore, prov)
		if err != nil {
			log.Fatalf("alcyona: start: %v", err)
		}
		defer app.Close()
		router.Swap(app)
		log.Printf("alcyona: normal mode (data %s)", dataDir)
	} else {
		// Setup mode: no server.toml yet. Serve only the wizard + /health.
		token, err := setupToken()
		if err != nil {
			log.Fatalf("alcyona: setup token: %v", err)
		}
		mgr := setup.New(dataDir, srvStore, prov, setup.NewSidecarManager(nil, nil))
		h := setup.NewHandler(mgr, token, func() {
			app, err := server.NewNormal(dataDir, srvStore, prov)
			if err != nil {
				log.Printf("alcyona: transition to normal mode failed: %v", err)
				return
			}
			registerClose(app.Close)
			router.Swap(app)
			log.Printf("alcyona: setup complete — now in normal mode")
		})
		mux := http.NewServeMux()
		h.Routes(mux)
		mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})
		router.Swap(mux)
		fmt.Printf("\nalcyona: setup mode — open the wizard:\n\n  http://%s:%d/setup?token=%s\n\n", *bind, *port, token)
	}

	addr := net.JoinHostPort(*bind, strconv.Itoa(*port))
	httpSrv := &http.Server{Addr: addr, Handler: router}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		log.Printf("alcyona: listening on %s", addr)
		errCh <- httpSrv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		log.Println("alcyona: shutting down")
	case err := <-errCh:
		log.Fatalf("alcyona: server: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("alcyona: shutdown: %v", err)
	}
	for _, f := range closeOnExit {
		f()
	}
}

// setupToken returns a random 128-bit token, hex-encoded.
func setupToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// closeOnExit holds teardown funcs for handlers swapped in after startup
// (e.g. the normal App created by the in-process setup transition).
var closeOnExit []func()

func registerClose(f func()) { closeOnExit = append(closeOnExit, f) }
