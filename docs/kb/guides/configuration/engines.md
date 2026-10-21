---
title: Sharing engine builds with engines
summary: Define each llama.cpp build once in engines and reference it from models with engine and args instead of repeating cmd.
category: guides
tags: [engines, cmd, configuration, llama-server, binary, build]
config_keys: [engines, engines.*.cmd, engines.*.label, engines.*.env, models.*.engine, models.*.args]
updated: 2026-10-21
---

# Sharing engine builds with `engines`

Different llama.cpp-family builds are faster for different models. When you
run more than one build, repeating the whole command per model means the
binary path and shared flags drift apart between models. The `engines`
section defines each build once; models reference it by name.

```yaml
engines:
  beellama:
    label: "beellama b12"
    cmd: |
      /opt/llama-builds/beellama/b12/llama-server
      --flash-attn
      --no-webui
    env:
      - "GGML_CUDA_ENABLE_UNIFIED_MEMORY=1"

models:
  saluki-27b:
    engine: beellama
    args: |
      --port ${PORT}
      --model /models/Saluki-27B-MTP.gguf
      --ctx-size 32768
```

The effective command is the engine's `cmd` followed by the model's `args`,
joined exactly as if you had written one multi-line `cmd`. Everything true
for `cmd` is true for the composed command — `guides/model-runtime/writing-cmd`
covers ports, proxies and comments; `guides/configuration/macros` covers
cutting repetition further.

## Which settings live where

- **engine `cmd`**: the binary and flags shared by every model on that
  build. Global macros work here.
- **model `args`**: everything specific to one model — `--model`, context
  size, `--port ${PORT}`. `${PORT}` and `${MODEL_ID}` work here because they
  are per-model values.
- **engine `env` / model `env`**: merged, and a model entry with the same
  name wins.
- Everything else (`proxy`, `cmdStop`, `ttl`, ...) stays on the model.

## What goes wrong

- `${PORT}` or `${MODEL_ID}` in the engine's `cmd` fails validation: engines
  are shared, so per-model macros cannot resolve there. Put them in the
  model's `args`.
- A model with `engine` and `cmd` at the same time is rejected — use one or
  the other.
- The engine binary must exist and be executable when the config loads.
  `llama-swap -validate` catches this before anything starts.
- A model using `args` without `engine` is rejected; `args` only appends to
  an engine command.

Models with a plain `cmd` keep working unchanged — `engines` is purely
opt-in. See `reference/config/engines` for every engine setting.
