---
title: Refining drafts with the draft assistant
summary: Enable draft_assistant and llama-swap discover --refine asks an OpenAI-compatible model (or llama-swap itself) to improve rule-based drafts; bad answers are rejected and never applied.
category: guides
tags: [discovery, draft-assistant, llm, configuration, cli, agent-api]
config_keys: [draft_assistant, draft_assistant.enabled, draft_assistant.api_base, draft_assistant.key_env, draft_assistant.model, draft_assistant.timeout, draft_assistant.max_tokens, draft_assistant.conventions]
updated: 2026-10-21
---

# Refining drafts with the draft assistant

The rule-based discovery drafts (`guides/configuration/discovery`) cover
the basics: the right engine, the model path, a capped `--ctx-size`.
Everything the rules cannot decide — quantization-aware flags, MoE
tuning, house conventions — is where the optional draft assistant comes
in. It sends the draft, the extracted GGUF metadata, the defined engines
and your conventions to any OpenAI-compatible chat endpoint and asks
for a refined stanza.

It is disabled by default, tooling-only (the running server's proxy and
swap behavior never reads it), and it only ever **produces a draft** —
nothing is applied, nothing is written.

```yaml
engines:
  beellama:
    label: "beellama b12"
    cmd: /opt/llama-builds/b12/llama-server --flash-attn

discovery:
  enabled: true
  folders:
    - /mnt/storage/LLM/models

draft_assistant:
  enabled: true
  api_base: https://api.openai.com/v1
  key_env: OPENAI_API_KEY
  model: gpt-4o-mini
  timeout: 60s          # optional, default 60s
  max_tokens: 2048      # optional, default 2048
  conventions: |        # optional, free text
    Prefer --flash-attn on every model that supports it.
    Keep --ctx-size at or below 32768 for Q4 quants.
```

- `enabled` (default `false`): without it, `llama-swap discover
  --refine` refuses to run and `POST /agent/v1/refine` answers 400.
- `api_base` (required when enabled): base URL of an
  OpenAI-compatible `/v1` endpoint; the call posts to
  `api_base/chat/completions`.
- `key_env` (required when enabled): the **name** of the environment
  variable holding the API key. The key itself is never stored in the
  config file; it is read at call time and never logged or echoed.
- `model` (required when enabled): the provider model name.
- `conventions` (optional): free-text house rules, sent to the model
  verbatim.

## No external provider required

`api_base` can point at llama-swap itself: start llama-swap with an
always-on small model, and let it refine the drafts for the big ones.

```yaml
models:
  qwen3-8b:
    cmd: /opt/llama-builds/b12/llama-server --port ${PORT} --jinja
          -m /mnt/storage/LLM/models/qwen3-8b-Q4_K_M.gguf

draft_assistant:
  enabled: true
  api_base: http://localhost:8000/v1   # this llama-swap's own proxy
  key_env: LLAMA_SWAP_SELF_KEY         # any non-empty value works
  model: qwen3-8b                       # a model ID from this config
```

## Running it

CLI (read-only, prints the refined stanza per file):

```bash
export OPENAI_API_KEY=sk-...
llama-swap discover -config /etc/llama-swap/config.yaml --refine
```

The report marks each draft:

```
   draft (paste under models:):
  tiny-q4-k-m:
    engine: beellama
    args: |
      --port ${PORT}
      --model /mnt/storage/LLM/models/tiny-Q4_K_M.gguf
      --ctx-size 32768
      --flash-attn
   refined: yes (draft_assistant)
```

`--json` adds a `refinements` array with one
`{path, modelId, refined, reason}` entry per drafted file. Over the
agent API (`guides/api-integration/agent-api`), `POST
/agent/v1/refine` with `{"path": "..."}` returns the draft plus
`refined` and `reason` fields.

## The answer is never trusted

The LLM's output goes through a hard gate before it replaces the
rule-based draft:

1. it must parse as YAML (code fences are tolerated);
2. it must be a single model stanza for the same model ID, and every
   field must be a real model field — unknown fields are rejected;
3. it must pass the **same validation as any config**: unknown engine,
   a stanza whose `${PORT}` composition is wrong, an invalid launch
   command — all rejected.

On any failure — parse, schema, validation, provider error or timeout —
the rule-based draft is used unchanged and the reason is reported:

```
   refined: no (fallback to rule-based draft: the refined stanza failed
   config validation: model tiny-q4-k-m references unknown engine "nope")
```

A missing `key_env` environment variable is a configuration error, not
a fallback: the CLI prints it and the agent API answers 422, because
fixing it is on you, not on the model.

## What goes wrong

- **`--refine requires an enabled draft_assistant section`**: the
  section is absent or `enabled` is false. Add it or drop the flag.
- **llama-swap refuses to start with `draft_assistant.api_base is
  required`** (or `key_env` / `model`): the section is enabled but
  incomplete; all three are required. `api_base` must be an absolute
  http(s) URL, `key_env` a valid environment variable NAME.
- **Every draft comes back `refined: no` with `provider call failed:
  ... context deadline exceeded`**: the provider is slower than
  `timeout`; raise it.
- **`refined: no ... does not match the model-stanza schema`**: the
  model is inventing fields. Tighten `conventions` ("answer with
  engine and args only") or pick a stronger model.
- **`draft assistant is misconfigured: environment variable ... is not
  set`**: the variable named by `key_env` is not exported in the shell
  (or systemd unit) running llama-swap.
