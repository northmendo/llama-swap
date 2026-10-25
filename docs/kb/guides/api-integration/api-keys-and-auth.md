---
title: API keys, access control, and keeping secrets out of your config
summary: apiKeys authentication has no per-key rate limits or user permissions; use env macros for secrets.
category: guides
tags: [security, api-keys, auth, secrets, env, rate-limit, users, permissions, access-control]
config_keys: [apiKeys, macros, peers.*.apiKey, models.*.env]
updated: 2026-10-25
---

# API keys and keeping secrets out of your config

## Requiring a key

By default llama-swap is unauthenticated. Add `apiKeys` and every request needs
one:

```yaml
apiKeys:
  - "sk-hunter2"
```

Clients may present it as an `Authorization: Bearer` header, an `x-api-key`
header, or HTTP Basic. Everything under `/api/` is covered too.

The web UI is split: the app itself loads without a key, but its API calls
need one. Every `GET` under `/ui/` — the app shell, its hashed JS/CSS
bundles, the SPA route fallbacks — is served without a key, because a
browser cannot attach one to a document or asset fetch. There is no API
route under `/ui/`; the handler only serves static UI files. The UI's own
data calls (`/v1/...`, `/api/...`, `/logs/...`) are gated like any other
client: on the first 401 the UI opens its key dialog, stores the key in
this browser's localStorage (the same stored key the Engines/Discovery
views use for `/agent/v1`), and attaches it as a Bearer header from then
on.

`/favicon.ico`, `/ui/site.webmanifest`, and the two icons it references
(`/ui/web-app-manifest-192x192.png`, `/ui/web-app-manifest-512x512.png`)
are covered by that `/ui/` exemption — and also stay reachable when you
serve the UI from somewhere else. Browsers fetch all of these themselves
— the favicon for the tab, the manifest and its icons to decide whether
to offer installing llama-swap as a PWA — and have no way to attach your
API key to those requests.

`GET /health` and `/health`-style liveness probes (`/wol-health`) also
stay public, so uptime checkers do not need the key.

**With no `apiKeys` at all, nothing changes**: every endpoint is open,
exactly as before, and the UI never asks for a key.

Generate a real one:

```console
$ printf "sk-%s\n" "$(head -c 48 /dev/urandom | base64)"
```

Multiple keys are allowed, which is how you rotate without downtime: add the
new key, move clients over, remove the old one.

All keys are equivalent. llama-swap has no per-key rate limiting, user
accounts, roles, or per-key permissions. Put a reverse proxy or API gateway in
front of llama-swap when you need those controls.

**`apiKeys` is not a substitute for a firewall.** llama-swap starts processes
on your machine. Do not expose it to the internet on the strength of a bearer
token alone.

## Keep the keys out of the file

Use env macros so the config itself is safe to commit:

```yaml
apiKeys:
  - "${env.LLAMA_SWAP_KEY}"
  - "${env.LLAMA_SWAP_KEY_ROTATE}"
```

`${env.VAR}` is substituted before anything else. **If the variable is not set,
config loading fails with an error** — which is what you want. A typo becomes a
startup failure rather than an instance running with an empty key list.

The same applies anywhere a secret appears:

```yaml
peers:
  openrouter:
    proxy: https://openrouter.ai/api
    apiKey: ${env.OPENROUTER_API_KEY}
    models: [z-ai/glm-4.7, moonshotai/kimi-k2-0905]

models:
  hf-model:
    env:
      - "HF_TOKEN=${env.HF_TOKEN}"
    cmd: llama-server --port ${PORT} -hf some/repo
```

## How peer keys are used

`peers.*.apiKey` is injected into outgoing requests to that peer, as **both**
`Authorization: Bearer <key>` and `x-api-key: <key>`. Leave it blank and no key
is added. It accepts a macro, so use `${env.*}`.

This is the key llama-swap presents *to* the peer. It is unrelated to the
`apiKeys` clients present *to* llama-swap.

## What ends up in your config file

Worth being aware of, because config files get pasted into issues and chats:

- `apiKeys` — literal keys, unless you used env macros
- `peers.*.apiKey` — literal peer keys, same
- `models.*.env` — literal `NAME=value` pairs
- `models.*.cmd` / `cmdStop` — full command lines, which often carry
  `--api-key` flags and absolute paths
- `macros` — **after** env substitution has already run

That last one is the non-obvious one: a macro like
`llama: "llama-server --api-key ${env.KEY}"` holds the resolved secret once the
config is loaded.

Before sharing a config, strip those. Redact rather than delete so the shape of
the problem survives.

## Related

- `reference/config/apiKeys`, `reference/config/peers`
- `guides/configuration/macros` — how env macros resolve
