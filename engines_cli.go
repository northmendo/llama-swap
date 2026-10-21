package main

// The `llama-swap engines` subcommand: list engines and their update
// state, check update sources, and update or roll back an engine's
// binary — Obtium-style, against the config file (and, when the server
// runs with the agent API enabled, its live model state).

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/engineupdate"
)

const enginesUsage = `usage: llama-swap engines <command> [name] [flags]

commands:
  list                every engine: label, binary, installed version,
                      and the update a check found
  check [name]        query update sources (all engines with a source
                      when no name is given), record state; no downloads
  update <name>       download, verify and swap the engine binary,
                      keeping the previous build for rollback
  rollback <name>     restore the previous build kept by update

flags:
  -config PATH        config file (required unless -config-dir is set)
  -config-dir DIR     directory of *.yml/*.yaml config files
  --force             update/rollback: stop models running the engine
                      instead of refusing (they stay stopped)
  -server URL         llama-swap server base URL used to check and stop
                      running models when agent_api is enabled
                      (default http://127.0.0.1:8080)

The updater state lives in engine-updates.json next to the config file.
An update refuses to swap while models using the engine are running;
when the server cannot confirm none are (agent_api disabled or the
server is down) --force is required to swap at all.
`

// runEngines implements `llama-swap engines`; it returns the exit code.
func runEngines(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("engines", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "path to config file")
	configDir := fs.String("config-dir", "", "directory of *.yml/*.yaml config files")
	force := fs.Bool("force", false, "stop models running the engine instead of refusing")
	serverURL := fs.String("server", "http://127.0.0.1:8080", "llama-swap server base URL (agent API)")

	if err := fs.Parse(args); err != nil {
		fmt.Fprint(out, enginesUsage)
		return 2
	}

	// Flags may appear anywhere: before the subcommand, between the
	// subcommand and the engine name, or after it. Walk the tokens,
	// parsing each flag run and collecting the positionals between.
	positional := fs.Args()
	collected := make([]string, 0, 2)
	for rest := positional; len(rest) > 0; {
		if err := fs.Parse(rest); err != nil {
			fmt.Fprint(out, enginesUsage)
			return 2
		}
		consumed := len(rest) - len(fs.Args())
		rest = fs.Args()
		if consumed == 0 && len(rest) > 0 {
			// rest[0] is a positional (flag parsing stopped on it).
			collected = append(collected, rest[0])
			rest = rest[1:]
		}
	}
	positional = collected
	if len(positional) == 0 {
		fmt.Fprint(out, enginesUsage)
		return 2
	}
	command := positional[0]

	var name string
	switch command {
	case "list":
		// name is meaningless for list; ignore extras below
	case "check":
	case "update", "rollback":
		if len(positional) < 1 {
			fmt.Fprintf(out, "engines %s: an engine name is required\n", command)
			return 2
		}
	default:
		fmt.Fprintf(out, "engines: unknown command %q\n\n%s", command, enginesUsage)
		return 2
	}
	if len(positional) > 1 {
		name = positional[1]
	}

	if *configPath == "" && *configDir == "" {
		fmt.Fprintln(out, "engines: at least one of -config or -config-dir must be provided")
		return 1
	}
	cfg, err := config.LoadConfigSources(*configPath, *configDir)
	if err != nil {
		fmt.Fprintf(out, "engines: config error: %v\n", err)
		return 1
	}

	stateSource := *configPath
	if stateSource == "" {
		stateSource = *configDir
	}
	manager := engineupdate.NewManager(cfg, engineupdate.DefaultStatePath(stateSource), cliRuntime(cfg, *serverURL))

	switch command {
	case "list":
		return runEnginesList(manager, out)
	case "check":
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var names []string
		if name != "" {
			names = []string{name}
		}
		results, err := manager.Check(ctx, names)
		if err != nil {
			fmt.Fprintf(out, "engines check: %v\n", err)
			return 1
		}
		return printCheckReport(results, out)
	case "update":
		res, err := manager.Update(context.Background(), name, *force)
		if err != nil {
			fmt.Fprintf(out, "engines update: %v\n", err)
			if engineupdate.IsRefusal(err) {
				return 3
			}
			return 1
		}
		version := res.Version
		if version == "" {
			version = "(unknown version)"
		}
		fmt.Fprintf(out, "engines update: %s updated to %s", name, version)
		if res.AssetName != "" {
			fmt.Fprintf(out, " (asset %s)", res.AssetName)
		}
		fmt.Fprintln(out)
		if len(res.StoppedModels) > 0 {
			fmt.Fprintf(out, "  stopped models (left stopped): %s\n", strings.Join(sortedCopy(res.StoppedModels), ", "))
		}
		fmt.Fprintf(out, "  previous binary kept for rollback\n")
		return 0
	case "rollback":
		res, err := manager.Rollback(name, *force)
		if err != nil {
			fmt.Fprintf(out, "engines rollback: %v\n", err)
			if engineupdate.IsRefusal(err) {
				return 3
			}
			return 1
		}
		fmt.Fprintf(out, "engines rollback: %s restored from %s\n", name, res.RestoredFrom)
		if len(res.StoppedModels) > 0 {
			fmt.Fprintf(out, "  stopped models (left stopped): %s\n", strings.Join(sortedCopy(res.StoppedModels), ", "))
		}
		return 0
	}
	return 2
}

// cliRuntime builds the running-models view for the CLI: the server's
// agent API when the config enables it, nothing otherwise (the manager
// then requires --force for any swap).
func cliRuntime(cfg config.Config, serverURL string) engineupdate.Runtime {
	if cfg.AgentAPI == nil || !cfg.AgentAPI.Enabled {
		return nil
	}
	return engineupdate.RemoteRuntime{
		BaseURL: strings.TrimRight(serverURL, "/"),
		Key:     cfg.AgentAPI.Key,
	}
}

func runEnginesList(m *engineupdate.Manager, out io.Writer) int {
	entries, err := m.List()
	if err != nil {
		fmt.Fprintf(out, "engines list: %v\n", err)
		return 1
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, "engines list: no engines defined")
		return 0
	}
	for _, e := range entries {
		version := e.InstalledVersion
		if version == "" {
			version = "(unknown version)"
		}
		line := fmt.Sprintf("%s  %q  %s  installed: %s", e.Name, e.Label, e.CmdPath, version)
		switch {
		case e.UpdateAvailable != "":
			line += fmt.Sprintf("  update available: %s", e.UpdateAvailable)
		case !e.HasUpdateSource:
			line += "  (no update source)"
		}
		fmt.Fprintln(out, line)
	}
	return 0
}

func printCheckReport(results []engineupdate.CheckResult, out io.Writer) int {
	if len(results) == 0 {
		fmt.Fprintln(out, "engines check: no engines with an update source")
		return 0
	}
	exitCode := 0
	for _, res := range results {
		switch {
		case res.Err != nil:
			fmt.Fprintf(out, "%s: check failed: %v\n", res.Engine, res.Err)
			exitCode = 1
		case res.Available:
			note := ""
			if res.Note != "" {
				note = " (" + res.Note + ")"
			}
			fmt.Fprintf(out, "%s: update available: %s%s\n", res.Engine, displayTag(res.Tag), note)
		case res.Note != "":
			fmt.Fprintf(out, "%s: %s\n", res.Engine, res.Note)
		default:
			fmt.Fprintf(out, "%s: up to date (%s)\n", res.Engine, displayTag(res.Tag))
		}
	}
	return exitCode
}

// displayTag renders a matched release tag, or "changed artifact" for
// direct URLs where no tag exists.
func displayTag(tag string) string {
	if tag == "" {
		return "changed artifact"
	}
	return tag
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
