---
title: Updating engine builds in place
summary: Give each engines entry an update source (GitHub releases or a direct URL) and check, update and roll back the binary with llama-swap engines.
category: guides
tags: [engines, update, rollback, github, checksum, operations]
config_keys: [engines, engines.*.update.source, engines.*.update.repo, engines.*.update.asset_regex, engines.*.update.tag_regex, engines.*.update.sha256, engines.*.update.url, engines.*.update.version, engines.*.update.api_base, agent_api.enabled]
updated: 2026-10-21
---

# Updating engine builds in place

With an `update:` block in an engines entry, llama-swap knows where new
builds of that engine come from — GitHub releases or a direct URL — and
can check for them, download, checksum-verify and swap the binary
without a manual download. The previous build is always kept, so a bad
release is one command away from being undone.

```yaml
engines:
  beellama:
    label: "beellama b12"
    cmd: |
      /opt/llama-builds/beellama/b12/llama-server
      --flash-attn
    update:
      source: github
      repo: "beellama/beellama"
      asset_regex: "llama-server-.*-linux-amd64"
      version: "b12"          # what is installed right now

  prismml:
    label: "PrismML nightly"
    cmd: /opt/llama-builds/prism/llama-server
    update:
      source: url
      url: "https://dl.example.com/prismml/llama-server"
      sha256: "5f2c...pinned-64-hex"   # REQUIRED for direct URLs
```

## The two sources

**GitHub** polls `{api_base}/repos/{repo}/releases` (the public API by
default; `api_base` overrides it for GitHub Enterprise Server). The
newest release whose tag matches `tag_regex` — or the newest
non-prerelease release when no `tag_regex` is set — wins, and the first
asset whose name matches `asset_regex` is the artifact. `sha256`
defaults to `auto`: the digest the GitHub release API exposes is
verified, and when a release ships none, the update is refused until
you pin `sha256` as 64 hex characters.

**url** fetches one direct artifact. A plain URL has no digest to
auto-trust, so a pinned `sha256` is **required** — `"auto"` is
rejected at config load. Because there is no version feed, checks
compare the ETag / Last-Modified the server served at the last update;
`version:` records what the pinned build is called.

## The commands

```
llama-swap engines list -config /etc/llama-swap/config.yaml
llama-swap engines check -config /etc/llama-swap/config.yaml
llama-swap engines update beellama -config /etc/llama-swap/config.yaml
llama-swap engines rollback beellama -config /etc/llama-swap/config.yaml
```

- `list` prints each engine's label, binary, installed version and the
  update a check found.
- `check` queries the sources and records what it found; it never
  downloads. Re-checking is idempotent.
- `update` downloads to a temp file **next to the binary**, verifies the
  checksum (a mismatch aborts with nothing touched), makes it executable
  and swaps it in with a rename. The previous build is kept at
  `<binary>.prev` — one slot, not a growing pile: a second update
  replaces the slot, and a rollback keeps the newer build in it, so
  both directions are reversible.
- `rollback` restores `<binary>.prev`.

State lives in `engine-updates.json` next to the config file (inside
`-config-dir` when you use one), recording per engine the installed
version, source, last check and last update.

## Running models refuse the swap

An update (or rollback) refuses while models using the engine are
running. With `--force` those models are **stopped first** — through
the server's normal unload path when the agent API is reachable — and
left stopped; llama-swap never silently restarts a model into an
unverified binary. Start them again yourself once the new build looks
good.

When the CLI cannot verify that nothing is running — the server is
down, or `agent_api` is not enabled — it refuses too, and `--force` is
the way to say "nothing is running, I checked". That is why a purely
offline update needs `--force`.

## The agent API surface

With `agent_api:` enabled, the same machinery is scriptable:
`GET /agent/v1/engines/updates` reads the recorded check results, and
`POST /agent/v1/engines/<name>/update` checks; a body of
`{"approved": true}` is required for any actual swap (with optional
`"force": true`), so an unapproved request can never replace a binary.

## What goes wrong

- `update.sha256 is required for url sources` — direct URLs must pin a
  checksum; there is no digest to auto-trust.
- `asset ... exposes no digest; pin sha256` — the matched GitHub
  release has no digest in its API record; pin the checksum instead.
- `checksum mismatch ... nothing was changed` — the artifact did not
  hash to what was expected; the binary and the rollback slot are
  untouched.
- `models ... are running and would be swapped under` — stop them or
  rerun with `--force`.
- `no rollback slot` — nothing to roll back to; run an update first.
- After a rollback the installed version is unknown until the next
  `check` re-syncs it from the source.
