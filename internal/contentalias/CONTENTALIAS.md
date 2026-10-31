# contentalias (fork-only)

The content-alias layer is a fork patch: upstream syncs never fix or guard it.

## Performance invariants

`Prepare` holds the unit's exclusive store lock (`Session.mu` plus the `map/lock`
flock) from `load` through alias allocation, schema compile, text encode, edit
apply, `json.Valid` and `save`. Every request on a DPX unit queues behind the
Prepare in flight, so the lock hold is the unit's request rate. Incident
t_129cf1ac: a quadratic `encodeText` held the lock 3.4-5.6 s on a 444 KB prompt
and serialized whole units for over an hour (bridge 600 s timeouts, 2,000+ 5xx).

Budgets, enforced by `lockhold_test.go` (runs in dpx-gates via lab_gate's
`alias_proofs` arm, `go test ./internal/contentalias`):

- Lock hold (`TestPrepareLockHoldBudget`) on synthetic 50/200/450 KB bodies
  generated in-test (`synthetic_fixture_test.go`: 60 tools, 60 messages, a
  manifest word and a codec literal every 2 KB): median < 100 ms on a warm map,
  < 300 ms on the first (cold) Prepare. The budget is generous for CI runners.
- N=16 concurrent Prepares on one unit (`TestPreparePerformanceUnderParallelCallers`):
  p95 <= 1.25 x the sum of the 16 store-lock holds recorded in that phase + one
  solo latency; the best of 7 solo Prepares costs <= 5x the thread CPU of 8
  `json.Valid` passes over the same body; and it spends < 40 ms blocked (wall
  minus thread CPU minus runqueue wait, `/proc/thread-self/schedstat`: sleep, I/O).
  Every bound is relative to the same run, so a loaded CI host stretches both
  sides alike (a fixed 800 ms cap failed on correct code on the ci-box, t_dae16eeb).
  Prepare serializes by design, so the bound is on the queue, not a ratio to N=1.
- 1x/4x/16x scaling (`TestPrepareScalesLinearly`), each axis alone (system text, tool catalog, message
  history) through Prepare, and text through both decode paths (`RestoreJSON`,
  the SSE `Stream`): 16x/1x <= 64 (linear ~16, quadratic ~256).

Mutation check: `python3 scripts/dpx/contentalias_lockhold_mutants.py` re-introduces
seven members of the class (unanchored encode scan, 50 ms sleep under the lock,
slow CPU work under `Session.mu` outside the flock, quadratic tool walk, linear symbol-allocation scan, quadratic JSON decode, SSE
history rescan) and exits 1 if any stays green. Run it on CI or ACE-AI; fleet
hosts deny local `go test`.

## History-only tool names (t_ca4ca1e2, t_63ed650a)

A transcript `tool_use.name` or `tool_reference.tool_name` can name a tool that
is absent from this request's `tools[]`: a tool the model hallucinated and the
caller answered "does not exist", a deferred or tool_search-loaded tool, or a
toolset that changed between turns. The transcript travels with every retry, so
refusing such a request kills the session on every seat.

Contract (option (a), name-only alias): `historyToolAlias` returns the stored
alias when the name is a declared tool. Otherwise it returns
`symbol(binding, "t", name)`, the same deterministic alias the name would get if
it were declared. The alias depends only on the binding and the name, so the
model sees one consistent alias across turns, and a later declaration of that
tool keeps it. The name goes into `Symbols` only. It is not added to `Tools`,
`RequestMap.allowed` or `reverse`, so a response `tool_use` or
`tool_reference` that names it is still refused with `unknown_tool`: the tool
was never offered, and the model cannot call it. Pass-through (option (b)) was
rejected because it would put a raw tool name on the wire.

`history_tool` remains only for shapes no alias can serve: an empty name, and
a name already shaped `dpx_v1_*` (a wire alias echoed back as a plain name).
A refused request leaves the persisted map unchanged. Tests:
`history_unknown_tool_test.go`.

## Near-copied tool aliases (t_7bd8c9b9)

The model mis-copies a `dpx_v1_t_<24 hex>` tool alias in about 0.16% of tool
calls (10 of 6,347, 2026-10-25..27; a 2-hex slip, or a tail rewritten as a
word such as `...648c7seed`). A response `tool_use` whose name is alias-shaped
(any name starting with the `dpx_v1_` codec prefix, `aliasShapedToolName`) but
not declared by this request now leaves DPX as
`unknown_tool_N` with input `{}`, on both the SSE `Stream` and `RestoreJSON`.
Before, it was a hard `unknown_tool` that failed the whole turn. The caller
answers with its own unknown-tool error, which lists its tools, and the model
re-calls. There is no nearest-neighbour repair, because a guess could run the
wrong tool.

The marker is a plain tool-name token (no parentheses), because the caller
stores it in history and a non-DPX fallback hop sends that history raw. It has
no `dpx_v1_` token, so the next request aliases it as a history-only name (see
above). It does not refuse with `history_tool`, and it stays non-callable.

The net is the whole `dpx_v1_` namespace, not `strayClassPattern`: a caller can
never declare a `dpx_v1_` tool (`reserved_tool`), so an undeclared one is always
a mangled alias. Narrower shapes missed live slips: `dpx_v1_dpx_v1_t_acorn_brick`
(prefix doubled, 2026-10-29, t_8621d5da) and `dpx_v1_stork_couch_placeholder`
for `dpx_v1_t_stork_couch` (class letter dropped, word tail added, 2026-10-30,
t_388a6308).

A name that is not alias-shaped (for example a raw `Bash`) is still a hard
`unknown_tool`. The model only sees aliases, so a raw name is a different fault
and should stay loud. Tests: `tool_alias_near_copy_test.go`,
`tool_alias_doubled_prefix_test.go`, `tool_alias_codec_prefix_test.go`. Mutation
check: `python3 scripts/dpx/contentalias_tool_marker_mutants.py` (7 mutants).

## Map v3: word-pair tool aliases (t_551a4f33)

Binding `version: v3` spells a tool alias the way v2 spells a property alias:
`dpx_v1_t_<word>_<word>`, two 5-letter words from `aliasWords`
(`toolalias.go`). Any two distinct word-pair aliases are at least 3 edits apart
(Hamming and Levenshtein), proved over all 145^2 = 21,025 aliases by
`TestV3AllWordPairsPairwiseDistance`, so a 1-2 character slip never names
another declared tool and falls to the `unknown_tool_N` marker above. The tail
is a closed word, so there is no open hex run for the model to finish as
English. Property aliases are unchanged from v2. The `dpx_v1_` prefix stays
(codec namespace, `strayClassPattern`, the marker net). v1 and v2 maps still
load with their 24-hex tool aliases; a v2 store does not open under a v3
binding, and the reverse.

Allocation is probe 0 of `toolSymbol(binding, name, k)` unless another tool
holds it, persisted in the map, so an alias never moves within a session. A
history-only name (section above) is allocated the same way and stays
non-callable.

Prompt cache: switching a unit to v3 changes every tool name once, so the first
request per session after the unit restarts misses the cached tool block. A unit
restart already mints a new session (new binding, every alias changes), so v3
adds no miss beyond the restart that deploys it. Steady state, a v3 alias is
20 bytes against 33: the 60-tool synthetic catalog's tool block is 780 bytes
smaller (`TestV3ToolPromptCost`). Roll it per unit with the row's
`alias_version: v3` (claude-dpx `studio/dpx_unit.py`, `ALIAS_VERSIONS`).

Tests: `tool_alias_v3_test.go`; `DPX_LIVE_MAPS=<dir> go test -run
TestV3ToolMinPairwiseDistance ./internal/contentalias` re-prepares every live
tool set under a v3 binding. The package suite runs on v3 by default
(`DPX_TEST_MAP_VERSION=v1|v2` re-runs it on an older version).
