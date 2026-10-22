---
title: Profiles and selectors
summary: Switch model sets at runtime with profiles, or resolve a virtual model ID per request with selectors.
category: guides
tags: [profiles, selectors, aliases, routing, virtual-models]
config_keys: [profiles, profiles.*.pins, selectors, selectors.*.strategy, selectors.*.targets, selectors.*.settings.cooldown]
updated: 2026-10-21
---

# Profiles and selectors

Both let a client ask for a stable model name and get a different real model
behind it. They solve different problems.

## Profiles: switch a whole set at once

A profile is a named set of model ID replacements. One profile, or none, is
active at a time, and you switch it from the web UI or the API.

```yaml
profiles:
  coding:
    description: "Coding-focused model routing"
    pins:
      llm-code: "qwen3-coder-30b"
      llm-plan: "gpt-oss-120b"
      image-gen: ~

  writing:
    description: "Long-form writing"
    pins:
      llm-code: "gemma-3-27b"
      llm-plan: "gemma-3-27b"
```

Your editor always requests `llm-code`. Flip the active profile and it gets a
different model, with no client-side change.

- Pin targets may be a local model, a fully qualified peer model, an alias, a
  `setParamsByID` alias, or a selector.
- An empty string or YAML `~` **disables** the pin — requests get a 404 and it
  is left out of model listings. Existing local, alias and peer IDs stay
  reachable.
- Pins are applied first, before aliases, filters and routing.
- Startup and config reload select **no** profile.

Switch at runtime:

```console
$ curl http://localhost:8080/api/profiles
$ curl -X PUT http://localhost:8080/api/profiles/active -d '{"name":"coding"}'
$ curl -X PUT http://localhost:8080/api/profiles/active -d '{"name":null}'
```

## Selectors: resolve per request

A selector is a virtual model ID resolved to a concrete target *on every
request*, based on a strategy.

```yaml
selectors:
  coding-model:
    strategy: warm
    targets: [gpt-oss-120b, qwen3-coder-30b]
    name: "Coding Model"
    description: "Best currently loaded coding model"
```

Four strategies:

**`warm`** — use the first target that is already ready; failing that, one that
is already starting; failing that, cold-start the first. This is the one you
want for "just use whatever is already loaded and don't make me wait".

**`pin`** — always the first target. The remaining entries are reference data
only. Useful as a stable indirection you can edit in one place.

**`spillover`** — fill each target up to a reservation count, then move to
the next. Good for spreading load across local and remote capacity:

```yaml
selectors:
  llama:
    strategy: spillover
    targets: [local-llama, peer-a/llama, peer-b/llama]
    settings:
      spillover: 4     # requests per active target before spilling over
```

**`failover`** — try the targets strictly in the listed order and retry the
next one when a target fails. This turns one model ID into a failure
cascade across hosted peers and local models, which is what you want when
a client should keep working through an outage with no client-side change:

```yaml
selectors:
  lila-brain:
    strategy: failover
    targets: [lunaroute/glm-5.3-background, deepseek/deepseek-v4-flash, ling-3.0-tiny]
    settings:
      cooldown: 90s
    name: "Lila brain cascade"
    description: "Lunaroute first, DeepSeek second, local ling last"
```

### `failover`: what counts as a failure

A target *fails* when any of the following happens **before the first
response byte has been forwarded to the client**:

- **Local model**: the model fails to load, or the health check times out.
- **Peer model**: the connection fails, the request times out, or the peer
  answers any HTTP status >= 400 — 401/403 (auth or billing), 429 (quota),
  5xx (transient) all count. These are exactly the ways a hosted provider
  rejects a request.

When a target fails, the same request is retried against the next target
with the model ID rewritten to it. If every target fails, the client
receives the **last** target's real error response, not a generic one.

Things that are deliberately **not** failures:

- **A 2xx answer that errors mid-response.** Once the first byte of a 2xx
  response has been forwarded, failover is impossible — the client is
  already reading the answer. A model that streams garbage and dies is
  surfaced as-is. (Corollary: a local model with `sendLoadingState` that
  streams a loading SSE commits the response early, so a subsequent load
  failure is reported in-band rather than triggering failover.)
- **A client disconnect.** If the client hangs up, there is nobody to fail
  over for; the walk stops.
- **Websocket upgrades.** An upgraded connection cannot be replayed, so it
  goes to the first eligible target without failover.

### `failover`: cooldown (circuit breaker)

After a target fails it is marked unhealthy and **skipped** for a cooldown
period — requests jump straight to the next target, so a dead primary adds
no latency to every request while it is down. The cooldown defaults to 60
seconds and can be overridden per selector:

```yaml
settings:
  cooldown: 90s    # a duration string: "30s", "90s", "1m30s", ...
```

- The cooldown expiry re-arms the target (it is tried again, and a
  success clears the mark immediately).
- If **every** target is inside its cooldown, the chain falls back to
  trying the targets in order anyway — the cooldown spares latency on a
  known-dead primary, it does not take a fully-degraded chain offline.
- Each failover logs a one-line event with the failing target, the reason,
  and the next target. The current per-target health is visible in the
  `llamaswap` metadata of the selector's `/v1/models` entry
  (`failover: [{target, healthy, retryAt, lastError}]`), so the web UI can
  show it — and it does: the Config page's Cascades panel renders it as
  green/red badges on the target chips, and the same selectors can be
  created, reordered and edited there (see
  `guides/api-integration/agent-api`, "Configuration verbs").

Rules worth knowing:

- Selector IDs cannot collide with model IDs, aliases or fully qualified peer
  model names.
- A selector's targets cannot be other selectors.
- Selectors are **not** supported on `/upstream/<model>` paths.
- Profiles run first, so a profile pin may target a selector.

## Which one?

**Profiles** are a mode switch you make deliberately — "I'm coding now". They
change several mappings at once and persist until you change them again.

**Selectors** are automatic, per request, and based on current state. Nobody
flips them.

They compose: pin `llm-code` in the `coding` profile to a `warm` selector, and
you get "when I'm coding, use whichever coding model is already hot".

## Related

- `reference/config/profiles` and `reference/config/selectors`
- `guides/routing/groups-and-matrix` — which models can be loaded at the same time
