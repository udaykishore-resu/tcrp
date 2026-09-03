package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/api"
	"github.com/udaykishore-resu/tcrp/internal/obs"
	"github.com/udaykishore-resu/tcrp/internal/platform"
)

func serve(ctx context.Context, args []string) error {
	fs := flagsFor("serve")
	addr := fs.String("addr", ":8080", "listen address")
	dir := fs.String("dir", "", "state directory (default: a temporary directory)")
	env := fs.String("env", "development", "environment name; a policy input")
	policyPath := fs.String("policy", "", "policy bundle to install instead of the embedded one")
	guardURL := fs.String("guard-url", "", "remote guardrail service; empty runs it in-process")
	model := fs.String("model", "planner", "model: planner or anthropic")
	modelID := fs.String("model-id", "", "model id when --model=anthropic")
	logLevel := fs.String("log", "info", "log level: debug, info, warn, error")
	traceFile := fs.String("trace-file", "", "write one JSON object per finished span to this file")
	devAuth := fs.Bool("dev-auth", true, "accept the X-TCRP-As development principal header")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *env == "production" && *devAuth {
		return errors.New("--dev-auth must be false when --env=production")
	}

	logger := obs.NewLogger(os.Stdout, *logLevel)
	p, err := platform.New(ctx, platform.Options{
		Dir: *dir, Environment: *env, PolicyPath: *policyPath, GuardURL: *guardURL,
		Model: *model, ModelID: *modelID, Logger: logger, TraceFile: *traceFile,
	})
	if err != nil {
		return err
	}
	defer p.Close()

	srv := api.NewServer(p, api.Options{
		Addr: *addr, Version: version, Commit: commit, BuiltAt: builtAt,
		DevPrincipalHeader: *devAuth, Logger: logger,
	})

	logger.Info("listening",
		"addr", *addr, "environment", *env, "state_dir", p.Dir(),
		"policy", p.Policy.Bundle().Version, "model", p.Model.Name(),
		"tools", p.Registry.Len())
	fmt.Fprintf(os.Stderr, "\n  dashboard  http://localhost%s/\n  api        http://localhost%s/v1/\n  metrics    http://localhost%s/metrics\n\n", *addr, *addr, *addr)

	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
