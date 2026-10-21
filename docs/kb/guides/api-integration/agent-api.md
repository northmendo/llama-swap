---
title: Driving model onboarding with the agent API
summary: Enable agent_api, hand an automation a bearer key, and it can scan folders, draft, validate and apply model stanzas over HTTP without touching YAML.
category: guides
tags: [agent-api, discovery, engines, configuration, automation, api]
config_keys: [agent_api, agent_api.enabled, agent_api.key]
updated: 2026-10-21
---

# Driving model onboarding with the agent API

Agents — LLM automations, home-assistant integrations, other people's
setups — can onboard models without hand-editing the config file. Enable
the `agent_api:` section and llama-swap mounts a JSON REST API at
`/agent/v1/` on its existing HTTP server that exposes the discovery and
config machinery:

```yaml
agent_api:
  enabled: true
  key: a-long-random-token
```

- `enabled` (default `false`): when false — or when the section is
  absent — no `/agent/v1` route exists at all and every request to one
  returns 404.
- `key` (required when enabled): the bearer token. `${env.VAR}` macros
  work, so `key: ${env.LLAMA_SWAP_AGENT_KEY}` keeps the token out of the
  file entirely.

**Warning:** treat the key like a password: it can rewrite your config
file. Bind llama-swap to localhost (`-listen localhost:8080`) unless the
agent is on another machine, and prefer the env-macro form of the key.

## Authentication

Every route requires `Authorization: Bearer <key>`. The key is compared
in constant time and never appears in responses, logs, error messages or
the redacted config output (`config__get_config` shows the whole
`agent_api` section as `[REDACTED]`). A missing, malformed or wrong
token all produce the same 401:

```bash
curl -s http://localhost:8080/agent/v1/engines
# {"error":{"message":"unauthorized: missing or invalid bearer token"}}

curl -s -H "Authorization: Bearer wrong-key" http://localhost:8080/agent/v1/engines
# same 401 — the response does not reveal how close the token was
```

## Routes

All requests and responses are JSON. Every error has the shape
`{"error": {"message": "..."}}`. Example below assume:

```bash
BASE=http://localhost:8080/agent/v1
AUTH="Authorization: Bearer a-long-random-token"
```

### POST /agent/v1/scan

Runs discovery over the configured `discovery.folders` (the
`discovery:` section and the `llama-swap discover` subcommand are covered
by `guides/configuration/discovery`). Read-only.

```bash
curl -s -X POST -H "$AUTH" $BASE/scan
```

```json
{
  "found": 1,
  "drafted": 1,
  "skipped": 0,
  "results": [
    {
      "path": "/models/tiny-Q4_K_M.gguf",
      "status": "drafted",
      "metadata": {
        "path": "/models/tiny-Q4_K_M.gguf",
        "fileSize": 1234,
        "ggufVersion": 3,
        "architecture": "llama",
        "type": "model",
        "contextLength": 4096,
        "blockCount": 12,
        "headCount": 16,
        "headCountKv": 4,
        "moe": false,
        "quantHint": "Q4_K_M"
      },
      "draft": {
        "modelId": "tiny-q4-k-m",
        "engine": "beellama",
        "engineSource": "default: first defined engine",
        "args": "--port ${PORT}\n--model /models/tiny-Q4_K_M.gguf\n--ctx-size 2048",
        "ctxSize": 2048,
        "ctxCapped": true,
        "yaml": "  tiny-q4-k-m:\n    engine: beellama\n    args: |\n      ..."
      }
    }
  ]
}
```

A file that cannot be drafted (unparseable GGUF, LoRA/projector files)
appears with `"status": "skipped"` and a `skipReason`. If the config has
no `discovery:` section the route returns 400.

### GET /agent/v1/engines

Lists the defined `engines:` (see `guides/configuration/engines`).
Environment variables are listed by name only — values are never
exposed, because engine env often carries credentials.

```bash
curl -s -H "$AUTH" $BASE/engines
```

```json
{
  "engines": [
    {
      "name": "beellama",
      "label": "beellama b12",
      "cmd": "/opt/llama-builds/b12/llama-server --flash-attn",
      "envKeys": ["GGML_CUDA_NO_PINNED_MEMORY"]
    }
  ]
}
```

### POST /agent/v1/draft

Drafts a stanza for one model file, exactly what `llama-swap discover`
produces for a single file. Read-only.

Request body:

```json
{"path": "/models/tiny-Q4_K_M.gguf"}
```

Response: the same `draft` object as in a scan result (shown above).
`404` when the file does not exist, `422` when it is not parseable as a
GGUF model, no engine can be picked, or the draft would not pass config
validation.

### POST /agent/v1/refine

Drafts a stanza for one model file and refines it — the HTTP form of
`llama-swap discover --refine`. The request body is the same
`{"path": "..."}` as draft. When the config enables the
`draft_assistant:` section (see `guides/configuration/draft-assistant`),
the refinement is LLM-assisted; without it the route answers `400`.

```bash
curl -s -X POST -H "$AUTH" -d '{"path": "/models/tiny-Q4_K_M.gguf"}' $BASE/refine
```

```json
{
  "modelId": "tiny-q4-k-m",
  "engine": "beellama",
  "engineSource": "draft_assistant refinement (model gpt-4o-mini)",
  "args": "--port ${PORT}\n--model /models/tiny-Q4_K_M.gguf\n--ctx-size 4096\n--flash-attn",
  "ctxSize": 4096,
  "ctxCapped": false,
  "yaml": "  tiny-q4-k-m:\n    engine: beellama\n    args: |\n      ...",
  "refined": true
}
```

The LLM's answer is never trusted: it must parse as YAML, match the
model-stanza schema and pass the same validation as any config. When it
does not — or the provider errors or times out — the response is still
a 200 with the **rule-based** draft, `refined: false` and the reason in
`reason`. Nothing is ever applied by this route; hand the stanza to a
human (or to `/agent/v1/validate` and `/agent/v1/apply`) afterwards.
A missing `draft_assistant.key_env` environment variable is a `422`,
not a fallback.

### POST /agent/v1/validate

Validates YAML with no side effects. The `yaml` field holds either:

- a **bare model stanza** — the shape drafts produce, pasted directly as
  the `models:` block. It is validated against the engines defined in
  the running server's config, so a stanza referencing a configured
  engine validates exactly like one written by hand;
- or a **full config document** (recognized by a top-level `models`
  key), which must stand on its own.

```bash
curl -s -X POST -H "$AUTH" -d '{"yaml": "my-model:\n  engine: beellama\n  args: |\n    --port ${PORT}\n    --model /models/tiny.gguf\n"}' \
  $BASE/validate
```

```json
{"valid": true, "errors": []}
```

A validation failure is a normal 200 with `valid: false` — the endpoint
answers the question rather than failing. Parse errors are reported the
same way:

```json
{"valid": false, "errors": ["model my-model references unknown engine \"nope\" (defined engines: beellama)"]}
```

### POST /agent/v1/apply

Merges one model stanza into the config file. Request body:

```json
{
  "yaml": "my-model:\n  engine: beellama\n  args: |\n    --port ${PORT}\n    --model /models/tiny.gguf\n",
  "approved": true
}
```

The flow is deliberately two-step: first send `approved` false (or omit
it) to check the stanza against the live config, then send the same
request with `approved: true` to write.

- **Invalid stanza** → `422` with `valid: false` and the errors;
  nothing is written, not even a backup.
- **Valid but `approved` is not `true`** → `200` with
  `"status": "pending"`, `"written": false`. Nothing is written; the
  API is stateless — no draft is stored server-side, the agent re-sends
  the stanza when the human approves.
- **`approved: true`** → the stanza is merged into the `models:` section
  of the config file:

```json
{
  "status": "applied",
  "model": "my-model",
  "approved": true,
  "written": true,
  "reloadRequired": true,
  "backup": "/etc/llama-swap/config.yaml.bak-20261021T193004Z",
  "message": "model merged into the config file; send SIGHUP (or restart) to load it, or run llama-swap with --watch-config for automatic reloads."
}
```

Safety contract:

- **Merge, never overwrite**: everything else in the file — other
  models, comments, ordering — is preserved; only the one model stanza
  is added. One cosmetic caveat: the file is re-rendered by a YAML
  round-trip, which keeps comments and key order but normalizes away
  blank lines between sections.
- **Backups**: before writing, the untouched original is copied to
  `<config>.bak-<UTC timestamp>` next to the config file.
- **Atomic writes**: the merged file is written to a temp file in the
  same directory and renamed into place. A failure at any point leaves
  the original config exactly as it was.
- **Existing models are never overwritten**: applying a model ID that is
  already defined returns `409`. Edit the file by hand (or remove the
  model) to change an existing model.
- **The merged file must load**: after merging, the whole document is
  run through the same validation as `llama-swap -validate` before
  anything is written. If the merge would corrupt the config (for
  example a duplicate alias), you get `422` and nothing is written.
- **Multiple stanzas are rejected**: apply takes exactly one model
  stanza, not a full config (`400`).

**Note:** llama-swap has no in-process reload trigger: it hot-reloads on
SIGHUP or on the --watch-config poll, both owned by the process's main
loop. Apply therefore reports reload_required — true unless you started
with --watch-config, in which case the file watcher reloads the merged
file automatically within its 2-second interval and apply reports
reload_required false.

`apply` needs a single config file: when llama-swap runs with only
`-config-dir`, the route answers `409` instead of guessing which file to
merge into.

## A full onboarding loop

```bash
# 1. see what is on disk
curl -s -X POST -H "$AUTH" $BASE/scan | jq '.results[0].draft.yaml'
# ... human reviews/edits the draft ...
# 2. dry-run it
curl -s -X POST -H "$AUTH" -d @edited-stanza.json $BASE/validate
# 3. write it
curl -s -X POST -H "$AUTH" -d @approved-stanza.json $BASE/apply
```

## What goes wrong

- **404 on every /agent/v1 route**: the `agent_api:` section is absent
  or `enabled` is false. Nothing is mounted, by design.
- **llama-swap refuses to start with "agent_api.key is required"**: the
  section is enabled but has no key. Keys must be a single token without
  whitespace (they travel in an Authorization header).
- **409 "apply needs a single config file"**: the server runs with
  `-config-dir` only; restart it with `-config <file>`.
- **422 "merged config would not load"**: the stanza is fine alone but
  collides with the live config (duplicate alias and friends); the file
  was not modified.
