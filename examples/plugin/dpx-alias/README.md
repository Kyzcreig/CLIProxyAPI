# dpx-alias: cpa brand-alias interceptor plugin

Aliases brand WORDS in the prompt content of every cpa vendor lane the policy
requires (Antigravity first), and restores them on the way back. Card
`t_a37235c0`; policy `t_8909e01d` (hermes-home `config/cpa-brand-scrub-policy.json`,
Obsidian "CPA Brand-Scrub Policy — lanes, exceptions, enforcement").

- Seam: `RequestInterceptor.InterceptRequestAfterAuth` (after credential pick,
  before executor translation) + `ResponseInterceptor` + `StreamChunkInterceptor`
  + `RequestLifecyclePlugin` (drops per-request state on `request.complete`).
- Core: `sdk/cliproxy/brandalias` (cgo-free, unit-tested in the main module's
  CI) over `internal/contentalias`'s word codec, the same aliaser DPX runs on
  the Claude native route. Symbols are `dpx_v1_w_<24hex>` / `dpx_v1_l_<24hex>`,
  a pure function of `(principal, session, word)`, so prompt-cache prefixes
  stay stable across turns.
- This directory is the C ABI shell only (`go/main.go`).

## What is and is not touched

| aliased (source-format body) | never touched |
|---|---|
| prose: `messages[].content`, `system`, `systemInstruction`/`contents[].parts[].text`, `tool_result` text | `model` (request and response echo) |
| custom tool names + descriptions, `tool_choice`, `allowedFunctionNames` | every header (the executor's vendor UA / identity headers are set after this seam) |
| tool-call names, ids and argument string values (history and response) | Antigravity envelope: `userAgent`, `requestType`, `project`, `sessionId`, `requestId` (written by the executor after this seam; proven byte-identical in `./test -run DPXAliasLane`) |
| `description` strings inside tool schemas | schema property keys, enums, `$ref`s (a brand word in a key stays and is COUNTED) |
| | signed blocks: Claude `thinking`/anything with `signature`, Gemini `thought` / `thoughtSignature` parts |
| | identity metadata (`metadata.user_id`, account ids, session ids), billing blocks |

Vocabulary (`brandalias.DefaultWords`): the fleet's harness/brand/component
names from the apx `bl-tokens` corpus that are WORDS. Agent names, the vendor
name and bare transport nouns are never in the manifest (standing ruling
2026-08-07; locked by `TestNeverScrubbedVocabulary`). The apx brand corpus (134
cases) is vendored as the gold set (`sdk/cliproxy/brandalias/testdata`).

## Lanes and policy (baked, not configurable)

| lane (raw executor lane) | policy | plugin |
|---|---|---|
| `antigravity` (ToFormat antigravity) | MUST be ON | aliased when listed in `lanes` |
| `gemini` (ToFormat gemini: gemini/vertex/aistudio creds) | MUST be ON | aliased when listed |
| `claude` (ToFormat claude, native route) | MUST be ON | aliased when listed; see the one-aliaser invariant below |
| `codex`, `xai` (ToFormat codex) | OFF | refused at configure time if listed; pass-through (`policy_exception`) |
| `kimi`, `openai-compatibility` (ToFormat openai) | OFF / relay loopback | pass-through (`policy_exception` / `openai_target_skipped`) |

**One aliaser per lane (claude).** The in-executor DPX aliaser
(`dpx-content-alias.enabled`, the Claude native route on the d-family units)
and this plugin are both aliasers of the claude lane. Invariant: **a cpa
instance lists `claude` in this plugin's `lanes` only when `dpx-content-alias`
is disabled on that instance.** The plugin cannot read the host config, so the
invariant is enforced by the hermes-home lint `cpa_brand_scrub_policy`
(`cpa_brand_scrub_double_alias` when two enabled aliasers cover one lane). On
the Studio main cpa `dpx-content-alias` is off; on the DPX units the plugin
must not list `claude`.

**openai-compatibility targets** (relay front doors on loopback: bpr/dlr/...)
are already scrubbed by apx/bpx. ToFormat `openai` resolves to no lane and
passes through, so the plugin can never double-alias them.

## Fail-closed rules

Unknown lane, unsupported source format (Responses API `openai-response` and
`interactions` are not walked in v1), a body that is not a JSON object, or any
error during the walk: the request leaves **byte-identical** and one wirelog row
names the reason. A vendor call is never refused. Response/stream inverse-map
failure returns the raw bytes (symbols can leak INBOUND only, never upstream)
plus a `phase: response|stream` row. A panic inside the shell is recovered as
a pass-through; the host additionally fuses a panicking plugin.

## Config

```yaml
plugins:
  enabled: true
  dir: /Users/alexgierczyk/.hermes/cliproxyapi/plugins
  configs:
    dpx-alias:
      enabled: true          # kill switch #1 (hot: PUT /v0/management/plugins/dpx-alias/enabled)
      priority: 10
      mode: shadow           # shadow (default: measure, never rewrite) | enabled
      lanes: [antigravity]   # kill switch #2 / rollout allowlist; codex/openai/xai/kimi are refused
      principal: studio-cpa  # symbol binding, operator-owned, keep stable
      session: main
      wirelog-spool: /Users/alexgierczyk/.hermes/cliproxyapi/state/dpx-alias-wirelog.jsonl
```

`plugins.configs.dpx-alias.enabled` + `lanes` is exactly what the hermes-home
lint reads to decide that a required lane is covered
(`aliasers.alias-plugin.plugin_names` includes `dpx-alias`, `lanes_key: lanes`).
A required lane missing from `lanes`, or the plugin disabled, is a lint FAIL
(`cpa_brand_scrub_required_uncovered`), which is the intended "plugin off on a
required lane goes red" check.

## Wirelog row (digest-only, no content, no headers)

```json
{"v":2,"capture":"cpa-alias","lane":"cpa-antigravity","mode":"shadow","reqId":"…","sourceFormat":"openai","toFormat":"antigravity","stream":false,"handled":false,"reason":"shadow","bytes":319,"brandTokensIn":6,"brandTokens":0,"symbols":5,"durationMs":0,"ts":"…","host":"…","id":"<pid>-<n>"}
```

`brandTokens` is counted over the WHOLE body that would leave (case-folded,
`-`/`_` removed), not only the prose, so a brand word left in a schema key or an
unwalked field shows up as a non-zero count instead of being hidden. `reason`
vocabulary: `ok`, `shadow`, `disabled`, `lane_not_enabled`, `policy_exception`,
`openai_target_skipped`, `unknown_lane`, `to_format_unset`,
`unsupported_source_format`, `invalid_json`, `walk_error`, `decode_error`,
`no_request_state`.

## Build and install

```bash
cd examples/plugin/dpx-alias/go
go build -buildmode=c-shared -o ../../bin/dpx-alias-v0.1.0.dylib .   # .so on linux
rm -f ../../bin/dpx-alias-v0.1.0.h
install -m 0644 ../../bin/dpx-alias-v0.1.0.dylib ~/.hermes/cliproxyapi/plugins/darwin/arm64/
```

The file stem `dpx-alias-v<version>` is the plugin id + version the host
selects on. Build the plugin from the same commit as the running cpa binary
(the RPC schema is pinned to `pluginabi.SchemaVersion`).

## Rollout (Antigravity first)

1. Install on the Studio with `mode: shadow`, `lanes: [antigravity]` and a
   wirelog spool. Run >= 24h; attach the rows to the card. Expected:
   `brandTokensIn > 0`, `brandTokens == 0`, `reason: shadow` for antigravity;
   `openai_target_skipped` / `policy_exception` for everything else; zero
   `walk_error` / `invalid_json`.
2. `mode: enabled` for the workers' cpa caller keys (`fleet-scripts`,
   `studio-probes`), then `hermes-cpa`, Apollo last. The plugin has no per-key
   scope today; stage callers by pointing them at the instance that runs
   `enabled` first (or by the order they are moved onto it).
3. Kill switch: `enabled: false` (hot via the management API) or drop the lane
   from `lanes`. Both leave every payload byte-identical immediately; in-flight
   responses of already-aliased requests still restore (state is per request).

**ACE-AI is plugin-less by policy** (`plugins.enabled: false` there). Its cpa
lanes are therefore NOT aliased; this is a documented asymmetry, not a drift
to fix here. The lint reports it as the open finding it is.

## Known residue (visible in `brandTokens`, by design)

- Schema property keys, enum values and `$ref` names that contain a brand word.
- Responses-API (`openai-response`) and `interactions` callers: pass-through.
- A symbol the model echoes that was NOT in the current request's prompt (for
  example after the client compacted history): restored only if still mapped;
  otherwise it reaches the client verbatim (never the vendor).
