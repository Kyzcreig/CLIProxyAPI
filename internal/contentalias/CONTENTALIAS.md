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
- N=16 concurrent Prepares on one unit (`TestPreparePerformanceUnderParallelCallers`): p95 <= 16 x 50 ms, and <= 3x the queue
  the measured single hold predicts. Prepare serializes by design, so the bound
  is on the queue, not a ratio to N=1.
- 1x/4x/16x scaling (`TestPrepareScalesLinearly`), each axis alone (system text, tool catalog, message
  history) through Prepare, and text through both decode paths (`RestoreJSON`,
  the SSE `Stream`): 16x/1x <= 64 (linear ~16, quadratic ~256).

Mutation check: `python3 scripts/dpx/contentalias_lockhold_mutants.py` re-introduces
six members of the class (unanchored encode scan, 50 ms sleep under the lock,
quadratic tool walk, linear symbol-allocation scan, quadratic JSON decode, SSE
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
