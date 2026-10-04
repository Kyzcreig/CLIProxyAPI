# Reset-Weighted Account Scheduler (Scheduler plugin)

Spend the quota that is about to be lost at the **weekly** reset FIRST, instead of
cpa's default even round-robin. A port of the Claude Relay Pool's reset-weighted
router (`~/.hermes/plans/2026-07-05_claude-relay-reset-weighted-router-SPEC.md` v0.4,
`claude-pool/claude_pool_lib.py` `Router` / `AffinityMap` / `fable_reserve_state`) to a
CLIProxyAPI **standard dynamic library plugin** (C ABI, `dlopen`'d in-process; the
only plugin shape the pinned fork's loader supports, and it needs a **cgo** build of
the proxy: `internal/pluginhost/loader_unix.go` is `//go:build cgo`).

Layout:

- `sdk/cliproxy/resetweighted/` — the brain (pure Go, no cgo): scoring, affinity,
  quota parsers, poller, config. Unit-tested; CI runs it (`go-test.yml`).
- `examples/plugin/reset-weighted-scheduler/go/` — the C ABI shell. Build:
  `cd examples/plugin/reset-weighted-scheduler/go && go build -buildmode=c-shared -o <plugins.dir>/<os>/<arch>/reset-weighted-scheduler-v0.1.0.dylib .`
  (`.so` on Linux; drop the generated `.h`).
- `test/reset_weighted_scheduler_test.go` — end-to-end: the plugin's pick is the
  Authorization the upstream receives, and the JSON pick line names the same auth id.

## Capabilities registered

`scheduler` (+ `scheduler_across_priorities`), `request_interceptor`, `usage_plugin`,
`management_api`.

- **Only ONE scheduler plugin can be live**: `Host.schedulerRecord()` returns the first
  active Scheduler-capable plugin. Install policy: this plugin must be the only
  Scheduler-capable plugin installed; do not install store plugins that carry the
  Scheduler capability or rewrite `priority` through `host.auth.save`
  (credential-priority, quota-pacer, credential-tier-router, antigravity-priority,
  claude-seat-pacer, quota-router...). They would silently replace or fight this one.
- `scheduler.pick` receives headers but **not the body**. Claude Code's session id lives
  in the body (`metadata.user_id`), so `request.intercept_before` derives it and injects
  `X-Rws-Session: <key>` (config `session_header`); pick reads that header, then falls
  back to `X-Session-ID` / `Session-Id`.
- A plugin pick **never seeds the host's affinity cache**, so the plugin owns affinity
  (below) and the host should run with `routing.session-affinity: false` once the
  plugin is `enabled`. cpa's affinity is the floor; the relay's is the target.
- An error envelope from pick **hard-fails the request** at the host. The plugin never
  returns one: every abnormal path (off, unknown provider, no candidates, panic)
  answers `Handled:false` and the built-in scheduler takes over (fail-open).
- Pick has no timeout and an escaped panic kills the proxy: pick reads memory only
  (the quota cache is filled by a background poller) and recovers at every entry
  point (`handleMethod`, `Engine.Pick`, `InterceptBefore`, `HandleUsage`).

## Scoring (per window span)

```
reclaim  = long_headroom x urgency            # the LONG window only (7d; Kimi 30d)
urgency  = clamp((horizon - time_to_reset) / horizon, 0..1)
horizon  = span x horizon_fraction            # 3/7 -> 72h on 7d, ~12.9d on 30d
score    = (weight_reclaim x reclaim + base_floor) / (1 + balance_k x inflight)
```

Guardrails (filters, not score terms): a vendor-`rejected`/capped window excludes the
seat; the 5-hour window at/over `short_guard_pct` (85) excludes the seat from NEW picks
while an alternative exists (INV-1: never empties the pool); unknown/stale quota
(`snapshot_max_age_s`, 900) -> reclaim 0, never "empty and safe"; ties within
`score_eps` break on the most-limiting-window headroom, then host priority, then id.

**Fable protection** (Ace 2026-10-03). The 7-day `seven_day_overage_included` (Fable)
bucket is the precious one. Ported `fable_reserve_state`: a seat is *reserved* when
`total_weekly_headroom <= fable_share x fable_headroom + margin`. In `enforce` mode a
non-Fable request may not take a reserved seat while an unreserved one exists, and a
Fable request ranks reserved seats first (a tier, not a magnitude), so each sub's Fable
headroom drains before its own weekly reset and no sub burns its Fable on non-Fable work
while another sub can take it. A Fable session binds under `<key>|fable` so a
mixed-model session cannot flap a binding. Compared against the store's quota-router
"protect claude-fable-* at 50% weekly": that flat rule withholds a 55%-used sub whose
Fable allowance is still mostly intact; ours does not (headroom-relative), and both
withhold the genuinely-stranded case (test `TestFableReservationVersusFlatFiftyPercentRule`).

## Affinity (richer than cpa's built-in)

`AffinityMap` (persisted at `affinity_path`, LRU `affinity_max`):

- **home**: the seat a session is bound to. Time never ends a binding; only quota
  exhaustion (seat `rejected`, a 429 on the seat via `usage.handle`, or a cap seen at
  pick) drops it. The 5-hour guard never evicts a binding.
- **sticky fallback**: when home is ineligible (transient outage, guard, Fable
  withhold, tried-set), the alternate the session landed on is remembered and reused
  on the next excursion (relay lesson t_4411fc15: fresh alternates cost a full
  prompt-cache write each time).
- **return home at most once**: home eligible again -> the session returns home once
  (`return_home`); if home flaps out and back within the same excursion the session
  **holds the fallback** (`hold_fallback`) instead of bouncing between two caches.

## Modes and the kill switch

`plugins.configs.reset-weighted-scheduler.mode`:

| mode | pick | logging | polling |
|---|---|---|---|
| `off` | `Handled:false` always | none | off |
| `shadow` (default, and the value for any unknown string) | `Handled:false`; host routes | one `{"event":"pick_shadow",...}` per selection with `chosen`, `host_first`, `differs` | on |
| `enabled` | `AuthID` | one `{"event":"pick",...}` per selection | on |

Kill switch = set `mode: off` (or `shadow`) in the durable config and hot reload. The
host re-reads `plugins.configs` on every config save/fsnotify event and calls
`plugin.reconfigure`; no restart. Verified on the bench: a `mode: enabled -> shadow`
edit took effect within ~4 s and the next request went through round-robin. Fleet
note: on the Studio the runtime copy is a tmp file (`launch-cliproxyapi.sh`); edit
`~/.hermes/cliproxyapi/config.yaml` through the deploy path, never by hand.

Deleting the dylib / `enabled: false` on the instance also removes the scheduler and
the host falls back to round-robin.

## Quota inputs

| provider | source | long window | short window |
|---|---|---|---|
| codex | `GET chatgpt.com/backend-api/wham/usage` (Bearer access_token + `ChatGPT-Account-Id`) | `primary_window` (7d; windows sorted by span) | `secondary_window` (5h) |
| xai | `GET cli-chat-proxy.grok.com/v1/billing?format=credits` | `creditUsagePercent`, `currentPeriod.end` (weekly) | none |
| kimi | `GET api.kimi.com/coding/v1/usages` (+ `X-Msh-*`) | `usages.limit_month_total` (30d) | `limits[]` 300-minute window |
| claude | `https://usage.ace/usage.json` schema 8, Claude rows | `seven_day` | `five_hour` (+ `seven_day_overage_included` = Fable) |
| antigravity | the same usage.ace feed is the intended source (the Connect-protocol quota summary is not polled by the plugin); parser for `gemini-weekly`/`gemini-5h` buckets ships | | |

cpa never polls cloud Claude subs directly (standing rule). Poll cadence
`poll_interval_s` floored at 300 s. Each provider's failure is isolated; a seat whose
quota is unknown or stale scores reclaim 0 and the pick falls back to host order
(fail-open). Tokens are read via `host.auth.get` for one GET each and never logged.
Claude rows map onto credentials by `claude_key_map` (auth id/email -> feed key) or by
the feed key appearing in the credential label/name/email (`sub-vps-6`).

Finding from the bench: `host.auth.list` exposes only **file-backed** credentials
(`internal/pluginhost/auth_callbacks.go` `buildHostAuthFileEntry` drops entries with
no `path` unless `runtime_only`). Config-only `codex-api-key` / `xai-api-key` entries
are therefore invisible to the poller and score as unknown (fail-open). The fleet's
credentials are auth files, so this does not bite the Studio/ACE-AI.

## Status endpoint

`GET /v0/resource/plugins/reset-weighted-scheduler/status` (and management route
`/v0/management/reset-weighted-scheduler/status`): per credential
`{headroom, urgency, reclaim, fable_reserved, fresh, exhausted, resets_at, observed_at,
bound_sessions}` plus counters `picks`, `shadow_picks`, `shadow_differs`,
`affinity_hits`, `declines`, `last_poll_at/error`.

## Config reference

```yaml
plugins:
  enabled: true
  dir: "/Users/alexgierczyk/.hermes/cliproxyapi/plugins"
  configs:
    reset-weighted-scheduler:
      enabled: true
      priority: 1
      mode: shadow                 # off | shadow | enabled
      fable_reserve_mode: enforce  # off | shadow | enforce
      fable_share: 0.5
      fable_reserve_margin_pct: 5
      horizon_fraction: 0.4285714  # 3/7
      short_guard_pct: 85
      balance_k: 0.5
      base_floor: 0.05
      score_eps: 0.02
      snapshot_max_age_s: 900
      poll_interval_s: 300
      usage_ace_url: "https://usage.ace/usage.json"
      claude_key_map: {}           # "<auth id or email>": "sub-vps-6"
      affinity_path: "/Users/alexgierczyk/.hermes/cliproxyapi/state/reset-weighted-affinity.json"
      affinity_max: 10000
      session_header: X-Rws-Session
      providers: [codex, claude, xai, kimi, antigravity]
      # bench only:
      # disable_polling: true
      # quota_seed: { "<id|label|email substring>": {long_used_pct: 13, long_resets_in: 7h, short_used_pct: 10} }
```

## Rollout (card t_c8754886)

1. Studio in `shadow` for >= 24 h (`routing.session-affinity` stays `true`): attach
   the `pick_shadow` excerpt and `shadow_differs / shadow_picks` from the status endpoint.
2. Studio `enabled` + `routing.session-affinity: false` (plugin owns affinity).
3. ACE-AI parity: its binary is a `cgo=0` cross build (`scripts/fleet-build.sh`) and
   cannot load any plugin; parity needs a native cgo build on ACE-AI first (t_78051fa7).

Rollback at every step: `mode: shadow` (or `off`) + hot reload; restore
`routing.session-affinity: true` when leaving `enabled`.

## Coexistence

Brand-scrub policy (Ace 2026-10-03): codex/xai/kimi are NOT aliased; this scheduler
assumes nothing about aliasing on any lane and coexists with the alias interceptor
plugin on claude/antigravity. It mutates nothing but the injected session header
(`intercept_before`) and reads the body only to derive that header.
