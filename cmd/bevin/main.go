// Command bevin is the single-binary Bevin assistant server: a web UI +
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
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/server"
	"github.com/utopiafallen/bevin/internal/setup"
	"github.com/utopiafallen/bevin/internal/store"
	"github.com/utopiafallen/bevin/web"
)

func main() {
	var (
		port = flag.Int("port", 0, "required: HTTP listen port (no default)")
		bind = flag.String("bind", "0.0.0.0", "bind host (default 0.0.0.0)")
		data = flag.String("data", "", "data dir (default: the directory containing this binary)")
	)
	flag.Parse()

	if *port <= 0 {
		fmt.Fprintln(os.Stderr, "bevin: -port is required (no default)")
		flag.Usage()
		os.Exit(2)
	}

	dataDir := *data
	var err error
	if dataDir == "" {
		dataDir, err = config.DefaultDataDir()
		if err != nil {
			log.Fatalf("bevin: cannot resolve binary dir: %v", err)
		}
	} else {
		dataDir, err = config.CleanPath(dataDir)
		if err != nil {
			log.Fatalf("bevin: bad -data: %v", err)
		}
	}

	srvStore, err := store.OpenServer(dataDir)
	if err != nil {
		log.Fatalf("bevin: open server store: %v", err)
	}
	defer srvStore.Close()
	prov := auth.NewInternalProvider(srvStore, time.Duration(config.DefaultSessionTTLSeconds)*time.Second)

	router := &server.Router{}

	if config.Exists(dataDir) {
		// Normal mode: server.toml already exists.
		app, err := server.NewNormal(dataDir, srvStore, prov)
		if err != nil {
			log.Fatalf("bevin: start: %v", err)
		}
		defer app.Close()
		router.Swap(app)
		log.Printf("bevin: normal mode (data %s)", dataDir)
	} else {
		// Setup mode: no server.toml yet. Serve only the wizard + /health.
		token, err := setupToken()
		if err != nil {
			log.Fatalf("bevin: setup token: %v", err)
		}
		mgr := setup.New(dataDir, *bind, *port, srvStore, prov, setup.NewSidecarManager(nil, nil))
		h := setup.NewHandler(mgr, token, func() {
			app, err := server.NewNormal(dataDir, srvStore, prov)
			if err != nil {
				log.Printf("bevin: transition to normal mode failed: %v", err)
				return
			}
			registerClose(app.Close)
			router.Swap(app)
			log.Printf("bevin: setup complete — now in normal mode")
		})
		mux := http.NewServeMux()
		h.Routes(mux)
		mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})
		// Serve the branding assets (theme, logo, font) in setup mode too, so
		// the wizard shares the same branding as the normal-mode UI. These are
		// explicit leaf routes (a "GET /" subtree would conflict with the
		// /setup/* routes in Go's ServeMux).
		if sub, err := fs.Sub(web.FS, "static"); err == nil {
			serveBrandingAsset(mux, sub, "/brand.css", "brand.css", "text/css; charset=utf-8")
			serveBrandingAsset(mux, sub, "/logo-mark.svg", "logo-mark.svg", "image/svg+xml")
			serveBrandingAsset(mux, sub, "/favicon.svg", "favicon.svg", "image/svg+xml")
			serveBrandingAsset(mux, sub, "/fonts/nunito-variable.woff2", "fonts/nunito-variable.woff2", "font/woff2")
		}
		router.Swap(mux)
		fmt.Printf("\nbevin: setup mode — open the wizard:\n\n  http://%s:%d/setup?token=%s\n\n", displayHost(*bind), *port, token)
	}

	addr := net.JoinHostPort(*bind, strconv.Itoa(*port))
	httpSrv := &http.Server{Addr: addr, Handler: router}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		log.Printf("bevin: listening on %s", addr)
		errCh <- httpSrv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		log.Println("bevin: shutting down")
	case err := <-errCh:
		log.Fatalf("bevin: server: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("bevin: shutdown: %v", err)
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

// serveBrandingAsset registers a single GET route for one embedded branding
// file (used in setup mode to share the logo/theme/font with the wizard). It
// uses an explicit leaf path rather than a "GET /" subtree, which would
// conflict with the /setup/* routes in Go's ServeMux.
func serveBrandingAsset(mux *http.ServeMux, sub fs.FS, pattern, file, contentType string) {
	b, err := fs.ReadFile(sub, file)
	if err != nil {
		return
	}
	mux.HandleFunc("GET "+pattern, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(b)
	})
}

// displayHost picks a connectable host for operator-facing URLs. Bound to all
// interfaces (0.0.0.0 / ::) → the machine's real network IP (what other hosts
// use to reach this server); a specific bind → that address. Falls back to
// 127.0.0.1 if no outbound IP is detectable.
func displayHost(bind string) string {
	switch bind {
	case "0.0.0.0", "::", "::0":
		if ip, err := lanIP(); err == nil && ip != "" {
			return ip
		}
		return "127.0.0.1"
	default:
		return bind
	}
}

// lanIP returns the machine's outbound IP without sending real traffic — a UDP
// "dial" only selects the local socket address from the routing table.
func lanIP() (string, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "", err
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "", errors.New("unexpected local address type")
	}
	return addr.IP.String(), nil
}

// closeOnExit holds teardown funcs for handlers swapped in after startup
// (e.g. the normal App created by the in-process setup transition).
var closeOnExit []func()

func registerClose(f func()) { closeOnExit = append(closeOnExit, f) }
