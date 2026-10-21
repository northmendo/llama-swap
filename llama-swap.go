package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mostlygeek/llama-swap/docs"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/discovery"
	"github.com/mostlygeek/llama-swap/internal/docagent"
	"github.com/mostlygeek/llama-swap/internal/draftassistant"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/hw"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/store/sqlite"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/mostlygeek/llama-swap/internal/tailcat"
	"github.com/mostlygeek/llama-swap/internal/watcher"
)

var (
	version = "0"
	commit  = "abcd1234"
	date    = "unknown"
)

const shutdownTimeout = 30 * time.Second

// docsConfigSchema stays at the repository root because it is the public
// schema URL and is also used by deployment tooling.
//
//go:embed config-schema.json
var docsConfigSchema []byte

// logTimeFormats maps the cfg.LogTimeFormat value to a Go time layout. An
// unset or unrecognised value yields "" — no timestamp prefix.
var logTimeFormats = map[string]string{
	"ansic":       time.ANSIC,
	"unixdate":    time.UnixDate,
	"rubydate":    time.RubyDate,
	"rfc822":      time.RFC822,
	"rfc822z":     time.RFC822Z,
	"rfc850":      time.RFC850,
	"rfc1123":     time.RFC1123,
	"rfc1123z":    time.RFC1123Z,
	"rfc3339":     time.RFC3339,
	"rfc3339nano": time.RFC3339Nano,
	"kitchen":     time.Kitchen,
	"stamp":       time.Stamp,
	"stampmilli":  time.StampMilli,
	"stampmicro":  time.StampMicro,
	"stampnano":   time.StampNano,
}

func configStorePath(cfg config.Config) string {
	if cfg.Store == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Store.Path)
}

func configureTailcatListener(cfg *config.Config, keyPath string) error {
	enabled := keyPath != ""
	cfg.SetTailcatEnabled(enabled)
	if !enabled {
		return nil
	}
	if cfg.Tailcat == nil || len(cfg.Tailcat.Models) == 0 {
		return fmt.Errorf("-listen-tailcat requires tailcat.models to define at least one exposed model")
	}
	if len(cfg.Tailcat.Allow) == 0 {
		slog.Warn(`tailcat.allow is empty: every Tailcat client will get 403 Forbidden; add client node keys, or "*" to allow any client`)
	}
	return nil
}

func loadTailcatListenerKey(keyPath string, enabled bool) (*tailcat.PrivateKey, error) {
	if !enabled {
		return nil, nil
	}
	privateKey, err := tailcat.LoadPrivateKey(keyPath, true)
	if err != nil {
		return nil, fmt.Errorf("load -listen-tailcat key: %w", err)
	}
	return privateKey, nil
}

// runValidate loads the configuration from the given sources and prints a
// short human-readable result to out. It returns 0 when the config loads
// without error and 1 otherwise. It does not start the server, detect
// hardware, or open a listener.
func runValidate(configPath, configDir string, out io.Writer) int {
	cfg, err := config.LoadConfigSources(configPath, configDir)
	if err != nil {
		fmt.Fprintf(out, "config validation failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "config is valid: %d model(s), %d peer(s)\n", len(cfg.Models), len(cfg.Peers))
	return 0
}

// runDiscover implements `llama-swap discover`: scan the config's
// discovery.folders for .gguf files and print, per file, the extracted
// GGUF metadata, the chosen engine (and why), and a draft model stanza that
// passes config validation. It reads only: the config file is never written
// and nothing is started. Exit code 0 when the scan ran; 1 on config
// errors or when no folders are configured.
//
// With --refine, each draft is additionally refined by the configured
// draft_assistant LLM (llama-swap discover --refine -config X). The
// refinement is a hard-gated draft: the LLM's answer must parse as YAML,
// match the model-stanza schema and pass the same config validation, or
// the rule-based draft is printed with the reason instead. --refine
// without an enabled draft_assistant section is an error (exit 1).
func runDiscover(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "path to config file")
	configDir := fs.String("config-dir", "", "directory of *.yml/*.yaml config files (additive to -config)")
	jsonOut := fs.Bool("json", false, "print the scan report as JSON")
	refine := fs.Bool("refine", false, "refine each draft with the configured draft_assistant LLM; falls back to the rule-based draft when the refinement fails")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if *configPath == "" && *configDir == "" {
		fmt.Fprintln(out, "discover: at least one of -config or -config-dir must be provided")
		return 1
	}

	cfg, err := config.LoadConfigSources(*configPath, *configDir)
	if err != nil {
		fmt.Fprintf(out, "discover: config error: %v\n", err)
		return 1
	}

	result, err := discovery.Run(cfg)
	if err != nil {
		fmt.Fprintf(out, "discover: %v\n", err)
		return 1
	}

	var refinements []refineNote
	if *refine {
		if cfg.DraftAssistant == nil || !cfg.DraftAssistant.Enabled {
			fmt.Fprintln(out, "discover: --refine requires an enabled draft_assistant section (see docs/kb/guides/configuration/draft-assistant.md)")
			return 1
		}
		refinements = refineScanDrafts(context.Background(), cfg, &result, out)
	}

	if *jsonOut {
		var payload any = result
		if *refine {
			payload = struct {
				discovery.ScanResult
				Refinements []refineNote `json:"refinements"`
			}{result, refinements}
		}
		encoded, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			fmt.Fprintf(out, "discover: encoding report: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, string(encoded))
		return 0
	}

	printDiscoverReport(result, refinements, out)
	return 0
}

// refineNote records the refinement outcome for one drafted file, in the
// human report and the --json output.
type refineNote struct {
	Path    string `json:"path"`
	ModelID string `json:"modelId"`
	Refined bool   `json:"refined"`
	Reason  string `json:"reason,omitempty"`
}

// refineScanDrafts runs the draft assistant over every drafted file of a
// scan, in place. A refined draft replaces the rule draft; a fallback
// keeps it. Config-class errors (key env var not set) are printed and
// also recorded as the note's reason — the scan itself still succeeds,
// because refinement is an enhancement, never a requirement.
func refineScanDrafts(ctx context.Context, cfg config.Config, result *discovery.ScanResult, out io.Writer) []refineNote {
	notes := make([]refineNote, 0, len(result.Results))
	for i := range result.Results {
		fr := &result.Results[i]
		if fr.Draft == nil || fr.Metadata == nil {
			continue
		}
		note := refineNote{Path: fr.Path, ModelID: fr.Draft.ModelID}
		outcome, err := draftassistant.Refine(ctx, cfg, *fr.Metadata, *fr.Draft)
		switch {
		case err != nil:
			fmt.Fprintf(out, "discover: refine %s: %v\n", fr.Path, err)
			note.Reason = err.Error()
		case outcome.Refined:
			*fr.Draft = outcome.Draft
			// The file-level engine source explains the engine the
			// refined draft actually references.
			fr.EngineSource = outcome.Draft.EngineSource
			note.Refined = true
		default:
			note.Reason = outcome.Reason
		}
		notes = append(notes, note)
	}
	return notes
}

// printDiscoverReport renders the human-readable per-file report: metadata,
// engine choice, draft stanza, skip reasons, refinement outcome, and the
// summary line.
func printDiscoverReport(result discovery.ScanResult, refinements []refineNote, out io.Writer) {
	refineByPath := make(map[string]refineNote, len(refinements))
	for _, note := range refinements {
		refineByPath[note.Path] = note
	}

	for _, fr := range result.Results {
		fmt.Fprintf(out, "== %s\n", fr.Path)

		if fr.Metadata == nil {
			fmt.Fprintf(out, "   metadata: unavailable: %s\n", fr.MetadataError)
		} else {
			m := fr.Metadata
			fmt.Fprintf(out, "   gguf v%d, arch %q", m.GGUFVersion, m.Architecture)
			if m.Type != "" {
				fmt.Fprintf(out, ", type %q", m.Type)
			}
			fmt.Fprintf(out, ", quant %q\n", quantOrUnknown(m.QuantHint))
			fmt.Fprintf(out, "   ctx %s, blocks %s, heads %s / kv %s, experts %s (used %s)%s\n",
				intOrUnknown(m.ContextLength), intOrUnknown(m.BlockCount),
				intOrUnknown(m.HeadCount), intOrUnknown(m.HeadCountKV),
				intOrUnknown(m.ExpertCount), intOrUnknown(m.ExpertUsedCount),
				moeTag(m.MoE))
			fmt.Fprintf(out, "   file size %d bytes\n", m.FileSize)
			if len(m.MTPKeys) > 0 {
				fmt.Fprintf(out, "   multi-token-prediction keys: %s\n", formatMTPKeys(m.MTPKeys))
			}
		}

		if fr.Draft != nil {
			fmt.Fprintf(out, "   engine: %s (%s)\n", fr.Draft.Engine, fr.EngineSource)
			if fr.Draft.CtxSize > 0 {
				capped := ""
				if fr.Draft.CtxCapped {
					capped = " (capped from " + intOrUnknown(fr.Metadata.ContextLength) + ")"
				}
				fmt.Fprintf(out, "   ctx-size: %s%s\n", intOrUnknown(fr.Draft.CtxSize), capped)
			}
			fmt.Fprintf(out, "   draft (paste under models:):\n%s\n", fr.Draft.YAML)
			if note, ok := refineByPath[fr.Path]; ok {
				if note.Refined {
					fmt.Fprintf(out, "   refined: yes (draft_assistant)\n")
				} else {
					fmt.Fprintf(out, "   refined: no (fallback to rule-based draft: %s)\n", note.Reason)
				}
			}
		} else {
			fmt.Fprintf(out, "   skipped: %s\n", fr.SkipReason)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "discovery: %s\n", result.SummaryLine())
}

func quantOrUnknown(hint string) string {
	if hint == "" {
		return "?"
	}
	return hint
}

func intOrUnknown(v int64) string {
	if v == 0 {
		return "?"
	}
	return fmt.Sprintf("%d", v)
}

func moeTag(moe bool) string {
	if moe {
		return " [MoE]"
	}
	return ""
}

func formatMTPKeys(keys map[string]any) string {
	names := make([]string, 0, len(keys))
	for name := range keys {
		names = append(names, fmt.Sprintf("%s=%v", name, keys[name]))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func main() {
	// `llama-swap discover` is a subcommand with its own flags; dispatch it
	// before the server's flag set so `discover -config X --json` parses.
	if len(os.Args) > 1 && os.Args[1] == "discover" {
		os.Exit(runDiscover(os.Args[2:], os.Stdout))
	}

	flagConfig := flag.String("config", "", "path to config file")
	flagConfigDir := flag.String("config-dir", "", "directory of *.yml/*.yaml config files (additive to -config)")
	flagListen := flag.String("listen", "", "listen address (default :8080 or :8443 for TLS)")
	flagCertFile := flag.String("tls-cert-file", "", "TLS certificate file")
	flagKeyFile := flag.String("tls-key-file", "", "TLS key file")
	flagListenTailcat := flag.String("listen-tailcat", "", "path to Tailcat server PrivateKey JSON file")
	flagVersion := flag.Bool("version", false, "show version and exit")
	flagWatchConfig := flag.Bool("watch-config", false, "reload config on file change")
	flagValidate := flag.Bool("validate", false, "validate the config file and exit (without starting the server)")
	flag.Parse()

	if *flagVersion {
		fmt.Printf("version: %s (%s), built at %s\n", version, commit, date)
		os.Exit(0)
	}

	if *flagConfig == "" && *flagConfigDir == "" {
		slog.Error("at least one of -config or -config-dir must be provided")
		os.Exit(1)
	}

	if *flagValidate {
		code := runValidate(*flagConfig, *flagConfigDir, os.Stdout)
		os.Exit(code)
	}

	useTLS := *flagCertFile != "" || *flagKeyFile != ""
	if (*flagCertFile != "" && *flagKeyFile == "") || (*flagCertFile == "" && *flagKeyFile != "") {
		slog.Error("both -tls-cert-file and -tls-key-file must be provided for TLS")
		os.Exit(1)
	}

	listenAddr := *flagListen
	if listenAddr == "" {
		if useTLS {
			listenAddr = ":8443"
		} else {
			listenAddr = ":8080"
		}
	}

	cfg, err := config.LoadConfigSources(*flagConfig, *flagConfigDir)
	if err != nil {
		slog.Error("failed to load config", "config", *flagConfig, "config-dir", *flagConfigDir, "error", err)
		os.Exit(1)
	}
	if err := configureTailcatListener(&cfg, *flagListenTailcat); err != nil {
		slog.Error("invalid Tailcat listener configuration", "error", err)
		os.Exit(1)
	}
	tailcatPrivateKey, err := loadTailcatListenerKey(*flagListenTailcat, cfg.TailcatEnabled())
	if err != nil {
		slog.Error("failed to load -listen-tailcat key", "error", err)
		os.Exit(1)
	}

	// Loggers are wired per cfg.LogToStdout: the selected streams feed
	// logs.MuxLogs, which owns the combined history served by /logs. They
	// outlive config reloads, so a LogToStdout change requires a restart to
	// take effect.
	logs, err := server.NewLoggers(cfg.LogToStdout)
	if err != nil {
		slog.Error("failed to create loggers", "error", err)
		os.Exit(1)
	}
	proxyLog := logs.ProxyLogs

	applyLogSettings := func(cfg config.Config) {
		level := logmon.LevelInfo
		switch strings.ToLower(strings.TrimSpace(cfg.LogLevel)) {
		case "debug":
			level = logmon.LevelDebug
		case "warn":
			level = logmon.LevelWarn
		case "error":
			level = logmon.LevelError
		}
		timeFormat := logTimeFormats[strings.ToLower(strings.TrimSpace(cfg.LogTimeFormat))]
		logs.SetLogLevel(level)
		logs.SetLogTimeFormat(timeFormat)
	}

	applyLogSettings(cfg)
	proxyLog.Debugf("PID: %d", os.Getpid())

	// Hardware describes the inference host and remains stable for the life of
	// this process, including config reloads. Detection is best effort so an
	// unavailable platform probe never prevents llama-swap from starting.
	var hardwareSnapshot *hw.HardwareSnapshot
	detectCtx, detectCancel := context.WithTimeout(context.Background(), 10*time.Second)
	detectedHardware, detectErr := hw.Detect(detectCtx, version)
	detectCancel()
	if detectErr != nil {
		proxyLog.Warnf("hardware detection unavailable: %v", detectErr)
	} else {
		hardwareSnapshot = &detectedHardware
	}

	// On Windows, bind the process tree to a Job Object so every upstream
	// process is reaped when llama-swap exits — even on a forced kill. No-op
	// elsewhere. Non-fatal: a failure just falls back to per-process teardown.
	if err := process.SetupTreeCleanup(); err != nil {
		proxyLog.Warnf("failed to set up process tree cleanup: %v", err)
	}

	// perfMon outlives config reloads; its config is updated in place.
	var perfMon *perf.Monitor
	if !cfg.Performance.Disabled {
		perfMon, err = perf.New(cfg.Performance, proxyLog)
		if err != nil {
			slog.Error("failed to create performance monitor", "error", err)
			os.Exit(1)
		}
		perfMon.Start()
	} else {
		proxyLog.Info("performance monitoring is disabled")
	}

	buildInfo := server.BuildInfo{Version: version, Commit: commit, Date: date}

	// Indexed once and shared by every Server instance, including the ones a
	// hot config reload creates: the documentation is immutable and does not
	// depend on cfg.
	referenceDocs := docagent.NewWithSchema(docs.Files, docsConfigSchema)

	initialStorePath := configStorePath(cfg)
	initialStore, err := sqlite.New(initialStorePath)
	if err != nil {
		slog.Error("failed to create store", "error", err)
		os.Exit(1)
	}

	initialSrv, err := server.New(cfg, logs, perfMon, initialStore, buildInfo, hardwareSnapshot, referenceDocs)
	if err != nil {
		slog.Error("failed to create server", "error", err)
		initialStore.Close()
		os.Exit(1)
	}
	initialSrv.SetConfigSource(*flagConfig, *flagWatchConfig)

	// activeSrv is swapped atomically during hot reload.
	var activeMu sync.RWMutex
	activeSrv := initialSrv
	activeStore := initialStore
	activeStorePath := initialStorePath

	tailcatHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		activeMu.RLock()
		srv := activeSrv
		activeMu.RUnlock()
		srv.ServeTailcatHTTP(w, r)
	})
	tailcatLogger := func(cfg config.Config) tailcat.Logger {
		if cfg.Tailcat != nil && cfg.Tailcat.Debug {
			return proxyLog
		}
		return nil
	}
	startTailcat := func(cfg config.Config) (*tailcat.Server, error) {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return tailcat.Start(ctx, tailcat.ServerOptions{
			PrivateKey: tailcatPrivateKey,
			Handler:    tailcatHandler,
			Logger:     tailcatLogger(cfg),
		})
	}

	var activeTailcat *tailcat.Server
	if cfg.TailcatEnabled() {
		activeTailcat, err = startTailcat(cfg)
		if err != nil {
			slog.Error("failed to start Tailcat server", "error", err)
			initialSrv.Shutdown(shutdownTimeout)
			initialStore.Close()
			os.Exit(1)
		}
	}
	if activeTailcat != nil {
		initialSrv.SetTailcatAddress(activeTailcat.Address())
		proxyLog.Infof("Tailcat listening on virtual TCP port 80: %s", activeTailcat.Address())
	}

	httpServer := &http.Server{
		Addr: listenAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			activeMu.RLock()
			srv := activeSrv
			activeMu.RUnlock()
			srv.ServeHTTP(w, r)
		}),
	}

	// reload guards against overlapping reloads triggered by concurrent signals
	// or file-watcher callbacks.
	var reloading bool
	var reloadMu sync.Mutex

	reload := func() {
		reloadMu.Lock()
		if reloading {
			reloadMu.Unlock()
			return
		}
		reloading = true
		reloadMu.Unlock()
		defer func() {
			reloadMu.Lock()
			reloading = false
			reloadMu.Unlock()
		}()

		proxyLog.Info("reloading configuration")

		newCfg, err := config.LoadConfigSources(*flagConfig, *flagConfigDir)
		if err != nil {
			proxyLog.Warnf("failed to reload config: %v", err)
			return
		}
		if err := configureTailcatListener(&newCfg, *flagListenTailcat); err != nil {
			proxyLog.Warnf("failed to reload config: %v", err)
			return
		}

		newStorePath := configStorePath(newCfg)
		activeMu.RLock()
		currentStore := activeStore
		currentStorePath := activeStorePath
		currentTailcat := activeTailcat
		activeMu.RUnlock()

		newStore := currentStore
		storeChanged := newStorePath != currentStorePath
		if storeChanged {
			newStore, err = sqlite.New(newStorePath)
			if err != nil {
				proxyLog.Warnf("failed to create new store during reload: %v", err)
				return
			}
		}

		newSrv, err := server.New(newCfg, logs, perfMon, newStore, buildInfo, hardwareSnapshot, referenceDocs)
		if err != nil {
			proxyLog.Warnf("failed to build new server during reload: %v", err)
			if storeChanged {
				newStore.Close()
			}
			return
		}
		newSrv.SetConfigSource(*flagConfig, *flagWatchConfig)

		if currentTailcat != nil {
			newSrv.SetTailcatAddress(currentTailcat.Address())
			// The listener is never restarted: connected Tailcat clients do not
			// reconnect to a restarted server. tailcat.allow is enforced by
			// newSrv per request; only the debug logger needs updating here.
			currentTailcat.SetLogger(tailcatLogger(newCfg))
		}

		activeMu.Lock()
		old := activeSrv
		oldStore := activeStore
		activeSrv = newSrv
		activeStore = newStore
		activeStorePath = newStorePath
		activeMu.Unlock()

		applyLogSettings(newCfg)
		if perfMon != nil {
			perfMon.UpdateConfig(newCfg.Performance)
		}

		if err := old.Shutdown(shutdownTimeout); err != nil {
			proxyLog.Warnf("error shutting down old server during reload: %v", err)
		}
		if storeChanged {
			if err := oldStore.Close(); err != nil {
				proxyLog.Warnf("error closing old store during reload: %v", err)
			}
		}

		// Notify UI after a short delay so it can refresh model state.
		time.AfterFunc(3*time.Second, func() {
			event.Emit(swaputil.ConfigFileChangedEvent{State: swaputil.ReloadingStateEnd})
		})

		proxyLog.Info("configuration reloaded")
	}

	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	defer watcherCancel()

	if *flagWatchConfig {
		proxyLog.Info("watching configuration for changes (poll-based, 2s interval)")

		if *flagConfig != "" {
			absConfigPath, err := filepath.Abs(*flagConfig)
			if err != nil {
				slog.Error("watch-config: failed to resolve config path", "error", err)
				os.Exit(1)
			}
			go func() {
				(&configwatcher.Watcher{
					Path:     absConfigPath,
					Interval: configwatcher.DefaultInterval,
					OnChange: reload,
				}).Run(watcherCtx)
			}()
		}

		if *flagConfigDir != "" {
			absConfigDir, err := filepath.Abs(*flagConfigDir)
			if err != nil {
				slog.Error("watch-config: failed to resolve config-dir path", "error", err)
				os.Exit(1)
			}
			go func() {
				(&configwatcher.DirWatcher{
					Path:     absConfigDir,
					Interval: configwatcher.DefaultInterval,
					OnChange: reload,
				}).Run(watcherCtx)
			}()
		}
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		var startErr error
		if useTLS {
			proxyLog.Infof("llama-swap listening with TLS on https://%s", listenAddr)
			startErr = httpServer.ListenAndServeTLS(*flagCertFile, *flagKeyFile)
		} else {
			proxyLog.Infof("llama-swap listening on http://%s", listenAddr)
			startErr = httpServer.ListenAndServe()
		}
		if startErr != nil && !errors.Is(startErr, http.ErrServerClosed) {
			slog.Error("http server error", "error", startErr)
			os.Exit(1)
		}
	}()

	if !swaputil.IsLoopbackAddr(listenAddr) {
		_, port, _ := net.SplitHostPort(listenAddr)
		proxyLog.Infof("llama-swap is reachable by all hosts on the network, use -listen localhost:%s to restrict to loopback only", port)
	}

	exitChan := make(chan struct{})

	go func() {
		for {
			sig := <-sigChan
			switch sig {
			case syscall.SIGHUP:
				proxyLog.Info("received SIGHUP, reloading config")
				go reload()
			case syscall.SIGINT, syscall.SIGTERM:
				proxyLog.Infof("received signal %v, shutting down", sig)
				watcherCancel()

				// Backstop against a stalled shutdown: force the process to
				// exit once the whole graceful sequence has had its full budget.
				// On Windows the Job Object reaps upstream processes on exit, so
				// a forced exit still cleans up rather than orphaning children.
				go func() {
					time.Sleep(shutdownTimeout + 5*time.Second)
					proxyLog.Warnf("graceful shutdown exceeded %v, forcing exit", shutdownTimeout)
					os.Exit(1)
				}()

				activeMu.RLock()
				srv := activeSrv
				st := activeStore
				tailcatRuntime := activeTailcat
				activeMu.RUnlock()

				// Close long-lived SSE streams first so httpServer.Shutdown can
				// drain without blocking on them for the full timeout.
				srv.CloseStreams()

				// Both phases share a single deadline so total shutdown is
				// bounded by shutdownTimeout rather than 2x it.
				deadline := time.Now().Add(shutdownTimeout)
				shutdownCtx, cancel := context.WithDeadline(context.Background(), deadline)
				defer cancel()
				if err := httpServer.Shutdown(shutdownCtx); err != nil {
					proxyLog.Warnf("http server shutdown error: %v", err)
				}
				if tailcatRuntime != nil {
					if err := tailcatRuntime.Close(shutdownCtx); err != nil {
						proxyLog.Warnf("Tailcat server shutdown error: %v", err)
					}
				}

				// Clamp the remaining budget to a small positive value: a
				// non-positive timeout makes the router fall back to its own
				// healthCheckTimeout, which would defeat the shared deadline.
				remaining := time.Until(deadline)
				if remaining <= 0 {
					remaining = time.Millisecond
				}
				if err := srv.Shutdown(remaining); err != nil {
					proxyLog.Warnf("router shutdown error: %v", err)
				}

				if perfMon != nil {
					perfMon.Stop()
				}
				if err := st.Close(); err != nil {
					proxyLog.Warnf("store shutdown error: %v", err)
				}

				close(exitChan)
				return
			}
		}
	}()

	<-exitChan
	proxyLog.Info("shutdown complete")
}
