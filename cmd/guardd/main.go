// Command guardd runs the guardrail as a standalone service.
//
// The same engine the platform links in-process, behind an HTTP interface, so a
// deployment can put the inspection path in its own failure domain, scale it
// independently, or run it as a sidecar next to several agent runtimes. The
// runtime does not know which it is talking to: guard.Inspector is satisfied by
// the local engine and by the client, and `tcrpd serve --guard-url` is the only
// difference between the two topologies.
//
// Running it separately costs a network hop on every turn. That is why the
// in-process path is the default and this binary exists for the deployments
// that need the isolation more than they need the microseconds.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/guard"
	"github.com/udaykishore-resu/tcrp/internal/identity"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

var (
	version = "dev"
	commit  = "none"
	builtAt = "unknown"
)

func main() {
	fs := flag.NewFlagSet("guardd", flag.ExitOnError)
	addr := fs.String("addr", ":8081", "listen address")
	budget := fs.Duration("budget", 100*time.Millisecond, "inspection latency budget")
	threshold := fs.Int("injection-threshold", 50, "injection score at which a finding becomes high severity")
	secret := fs.String("signing-secret", "", "verify signed principal assertions with this secret; empty accepts unauthenticated calls")
	logLevel := fs.String("log", "info", "log level: debug, info, warn, error")
	showVersion := fs.Bool("version", false, "print build information and exit")
	_ = fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Printf("guardd %s (commit %s, built %s)\n", version, commit, builtAt)
		return
	}

	logger := obs.NewLogger(os.Stdout, *logLevel)
	clock := obs.RealClock{}
	reg := obs.NewRegistry()
	tracer := obs.NewTracer("tcrp-guard", clock)

	cfg := guard.DefaultConfig()
	cfg.Budget = *budget
	cfg.InjectionThreshold = *threshold
	engine := guard.NewEngine(cfg, clock, tracer, reg)

	var signer *identity.Signer
	if *secret != "" {
		signer = identity.NewSigner(*secret, 15*time.Minute)
	} else {
		logger.Warn("no signing secret configured; the service will accept unauthenticated inspection requests")
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/", guard.Handler(engine, signer, clock))
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(reg.Render()))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","policy":%q,"budget_ms":%d}`, cfg.Version, cfg.Budget.Milliseconds())
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("listening", "addr", *addr, "policy", cfg.Version, "budget", cfg.Budget.String())
	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		fmt.Fprintf(os.Stderr, "guardd: %v\n", err)
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "guardd: %v\n", err)
			os.Exit(1)
		}
	}
}
