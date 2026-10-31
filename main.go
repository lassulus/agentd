// agentd runs coding agents (anything that speaks ACP on stdio) and exposes
// them through a REST API.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/agentd/config.json", "config file")
	tokensFile := flag.String("tokens-file", "", "API tokens file (overrides auth.tokensFile)")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	if *tokensFile == "" {
		*tokensFile = cfg.Auth.TokensFile
	}
	auth, err := newAuthenticator(cfg, *tokensFile)
	if err != nil {
		slog.Error("auth", "err", err)
		os.Exit(1)
	}
	m, err := newManager(cfg)
	if err != nil {
		slog.Error("state", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{Addr: cfg.Listen, Handler: (&server{m: m, auth: auth}).routes()}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		m.StopAll()
		_ = srv.Close() // not Shutdown: open SSE streams would block it
	}()
	slog.Info("listening", "addr", cfg.Listen, "agents", len(m.List()))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}
