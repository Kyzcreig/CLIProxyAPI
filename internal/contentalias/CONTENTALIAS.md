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
