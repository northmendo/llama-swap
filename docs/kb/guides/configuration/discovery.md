---
title: Drafting model stanzas with discovery
summary: Point discovery.folders at your GGUF folders and llama-swap discover prints validated draft stanzas with engine, model path and ctx-size filled in.
category: guides
tags: [discovery, gguf, engines, configuration, cli]
config_keys: [discovery, discovery.enabled, discovery.folders, discovery.ctxSizeCap, discovery.engine_rules, discovery.engine_rules.*.match, discovery.engine_rules.*.field, discovery.engine_rules.*.engine]
updated: 2026-10-21
---

# Drafting model stanzas with discovery

Every new model usually means hand-writing a stanza with the right engine,
`--model` path and a context size you have to go looking for. The
`discovery:` section plus the `llama-swap discover` subcommand does the
boring part: it scans your folders for `.gguf` files, reads each file's
GGUF metadata (pure read, nothing is started, nothing is written), picks an
engine, and prints a draft stanza you paste under `models:`.

```yaml
engines:
  fastbuild:
    label: "llama.cpp b4321"
    cmd: /opt/llama-builds/b4321/llama-server --flash-attn
  bigbuild:
    label: "llama.cpp bigctx"
    cmd: /opt/llama-builds/bigctx/llama-server --flash-attn --jinja

discovery:
  enabled: true
  folders:
    - /mnt/storage/LLM/models
  ctxSizeCap: 32768
  engine_rules:
    - match: "MTP"
      engine: bigbuild
    - match: "qwen3"
      field: architecture
      engine: fastbuild
```

Then run:

```bash
llama-swap discover -config /path/to/config.yaml
# or, for machine-readable output:
llama-swap discover -config /path/to/config.yaml --json
```

For every `.gguf` file found it prints the path, the extracted metadata
(architecture, trained context, block count, attention heads, expert counts
for MoE detection, multi-token-prediction keys when present, the quant hint
parsed from the filename, file size), the engine it chose and why (which
`engine_rules` entry matched, or the default), and the draft stanza:

```yaml
  underdog-saluki-27b-1.0-iq2-mix-mtp:
    engine: bigbuild
    args: |
      --port ${PORT}
      --model /mnt/storage/LLM/models/Kujira/Underdog-Saluki-27B-1.0-MTP/Underdog-Saluki-27B-1.0-IQ2-mix-MTP.gguf
      --ctx-size 32768
```

Paste it under `models:`, adjust anything you want, and you are done. Every
draft is run through the same validation as a hand-written stanza before it
is shown, so what you paste loads.

## How the pieces are chosen

- **Engine**: `engine_rules` are evaluated top to bottom, first match wins.
  `match` is a Go regex tested against the file's base name (default) or,
  with `field: architecture`, the GGUF `general.architecture` value (e.g.
  `qwen3`). With no matching rule the first engine *defined in the YAML
  document* is used.
- **ctx-size**: `min(trained context length, ctxSizeCap)` — trained lengths
  like 262144 get capped at the default 32768; a file without a context
  length gets no `--ctx-size` line at all.
- **Model ID**: the filename lowercased with underscores/spaces turned into
  dashes, quant tag included (`Saluki-27B-Q4_K_M.gguf` becomes
  `saluki-27b-q4-k-m`).

## What goes wrong

- A folder that does not exist or is not readable fails config load and
  `llama-swap -validate` — bad paths are reported before any scanning.
- A rule whose `engine` names an engine that is not defined is a config
  error, caught at load.
- With no `engines:` section at all, discovery reports why it cannot draft
  and skips the file instead of guessing.
- Files that are not chat models are reported and skipped: LoRA adapters
  (`general.type: adapter`), vision projectors (`general.type: mmproj`) and
  files that are not valid GGUF. The summary line shows
  `N found / M draftable / K skipped`.

Discovery never writes your config file and never starts a server or a
model — it is a read-only drafting tool, and the running server ignores the
`discovery:` section.
