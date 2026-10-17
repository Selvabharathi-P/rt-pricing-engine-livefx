// Command simulator imitates the LiveFXHub market feed so the engine can be
// developed, demoed and benchmarked without production credentials.
//
// Settings come from the `simulator:` section of config.yaml (CONFIG_FILE,
// else ./config.yaml, else /etc/pricing-engine/config.yaml); SIM_LISTEN_ADDR,
// SIM_MODE, SIM_TICK_INTERVAL and SIM_DROP_EVERY override it. The accepted
// credentials are read from the env vars named by api_key_env/api_secret_env
// (default demo-key / demo-secret).
//
// mode: livefxhub reproduces the real feed's protocol and messages (see
// server.go); mode: legacy sends the assignment brief's format.
package main

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"pricing-engine/internal/config"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg, err := config.LoadSimulator()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.Handle(cfg.Path, newSim(cfg, log))
	log.Info("simulator listening", "addr", cfg.Listen, "path", cfg.Path, "mode", cfg.Mode,
		"tick", cfg.TickInterval.String(), "update_ratio", cfg.UpdateRatio, "one_session_per_key", cfg.OneSessionPerKey,
		"silent_symbols", strings.Join(cfg.SilentSymbols, ","))
	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}
