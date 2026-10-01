"""Hermetic lab gate for a `fleet` checkout of CLIProxyAPI (spec one-cliproxyapi-lineage D5,
§7 Phase 2; ported from ANG-Ventures/claude-dpx scripts/lab_gate.py, which ran it against a
materialized tree — on `fleet` there is nothing to materialize).

Runs in the fork's CI (.github/workflows/dpx-gates.yml) and by hand on a fleet host. No
provider egress: every CLI run is sandboxed to localhost with a synthetic token.

Gates (all must pass for green):
  build            go build ./cmd/server and ./cmd/dpx-alias-offline
  alias_proofs     go vet + go test contentalias, config, executor (stock suite + DPX alias/cch
                   + daemon-shape tests), the HTTP entrypoint test, the AC-M12 determinism golden,
                   TestFleetAliasOffClaudeWire (50 replays, alias off == pristine <BASE>) and
                   the DPX-shape no-policy-metadata / policy-not-off refusal tests
  wire_capture     genuine pinned CLI TUI -> dpx-alias-offline -> loopback upstream
                   (cch_capture.py): 0 brand strings upstream, block present with
                   cc_entrypoint=cli, cch valid over the sent body, upstream User-Agent equals
                   the CLI's own UA
  signer_equiv     the pinned CLI signs its own body (first-party flag, loopback); CPA's signer
                   reproduces the CLI's digits byte-for-byte
  cli_window       CPA's native-client detector confirms the pinned CLI (else DPX refuses
                   every request with native_route_required)
  model_catalog    every fleet Claude model id is in the build's embedded catalog (else CPA
                   answers 400 unknown provider for model)

The three CLI arms need the pinned Claude Code binary, macOS sandbox-exec and claude-apx's
cch.js, which a GitHub runner does not have. With --no-cli each of them runs its HERMETIC
form instead and says so in summary.json (`mode: hermetic-no-cli`):
  wire_capture     TestDPXAliasResignsStockCLIBlock + TestDPXAliasExecutorHTTP: a stock
                   CLI-shaped body through the real executor with alias ON -> block forwarded
                   with cc_entrypoint=cli, 0 brand strings upstream, UA preserved, response
                   reverse-mapped
  signer_equiv     the same test's assertion that the sent cch equals
                   signAnthropicMessagesBody over the sent body (CPA's signer, the one
                   signer_equiv compares to the CLI's digits when a CLI is present)
  cli_window       the coupling probe's version window: the pinned CLI version's User-Agent
                   is plausible to the stock detector (DPX_COUPLING_WINDOW_EXTRA)
The genuine-CLI forms stay the fleet-host run (update lane, Phase 5).

Writes <out>/summary.json and exits 0 green / 1 red / 2 usage.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent

FLEET_TEST_RUN = '|'.join([
    'TestDPXAliasExecutorHTTP', 'TestDPXAliasDeterminismGolden',
    'TestFleetAliasOffClaudeWire', 'TestFleetAliasOnDPXShape',
])


def run(cmd, cwd=None, env=None, timeout=1800):
    t0 = time.time()
    try:
        p = subprocess.run(cmd, cwd=cwd, env=env, capture_output=True, text=True, timeout=timeout)
        rc, out = p.returncode, (p.stdout + p.stderr)
    except subprocess.TimeoutExpired as exc:
        rc, out = 124, 'timeout after %ss: %s' % (timeout, exc)
    return rc, out[-4000:], round(time.time() - t0, 1)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--source', type=Path, default=ROOT, help='fleet checkout (default: this repo)')
    ap.add_argument('--out', type=Path, required=True)
    ap.add_argument('--cli-version', required=True, help='fleet CLI pin, e.g. 2.1.283')
    ap.add_argument('--cli-dir', type=Path, default=Path.home() / '.local/share/claude/versions')
    ap.add_argument('--models', default='', help='comma-separated fleet Claude model ids')
    ap.add_argument('--cch-js', type=Path, default=Path.home() / 'Projects/claude-apx/src/cch.js')
    ap.add_argument('--skip-wire', action='store_true', help='skip the TUI wire capture (a CLI host without a TTY)')
    ap.add_argument('--no-cli', action='store_true', help='no CLI binary on this host: run the hermetic forms of the CLI arms (CI)')
    args = ap.parse_args()
    src = args.source.resolve()
    out = args.out.resolve()
    out.mkdir(parents=True, exist_ok=True)
    os.chmod(out, 0o700)
    gates = {}
    env = dict(os.environ, CGO_ENABLED='0')

    # build
    b1 = run(['go', 'build', '-o', str(out / 'dpx-server'), './cmd/server'], cwd=src, env=env)
    b2 = run(['go', 'build', '-o', str(out / 'dpx-alias-offline'), './cmd/dpx-alias-offline'], cwd=src, env=env)
    gates['build'] = {'pass': b1[0] == 0 and b2[0] == 0, 'rc': [b1[0], b2[0]], 'secs': [b1[2], b2[2]],
                      'tail': (b1[1] if b1[0] else '') + (b2[1] if b2[0] else '')}

    # alias proofs
    steps = [
        ['go', 'vet', '-composites=false', './internal/contentalias', './internal/runtime/executor', './internal/config'],
        ['go', 'test', './internal/contentalias', '-count=1'],
        ['go', 'test', './internal/config', '-count=1'],
        ['go', 'test', './internal/runtime/executor', '-count=1'],
        ['go', 'test', './test', '-run', FLEET_TEST_RUN, '-count=1'],
    ]
    res = [run(s, cwd=src, env=env) for s in steps]
    gates['alias_proofs'] = {'pass': all(r[0] == 0 for r in res), 'rc': [r[0] for r in res],
                             'secs': [r[2] for r in res], 'tail': ''.join(r[1] for r in res if r[0])}

    cli = args.cli_dir / args.cli_version
    have_cli = cli.exists() and not args.no_cli
    missing = {'pass': False, 'reason': 'pinned CLI %s not installed at %s (install it: `claude install %s`, or --no-cli)'
               % (args.cli_version, cli, args.cli_version)}
    # wire capture through the built DPX harness
    if args.no_cli:
        rc, tail, secs = run(['go', 'test', './internal/runtime/executor', '-run', 'TestDPXAliasResignsStockCLIBlock', '-count=1', '-v'], cwd=src, env=env)
        rc2, tail2, secs2 = run(['go', 'test', './test', '-run', 'TestDPXAliasExecutorHTTP', '-count=1'], cwd=src, env=env)
        runs = tail.count('--- PASS: TestDPXAliasResignsStockCLIBlock/')
        gates['wire_capture'] = {'pass': rc == 0 and rc2 == 0 and runs >= 4, 'mode': 'hermetic-no-cli',
                                 'checks': {'resign_runs': runs, 'http_entrypoint': rc2 == 0}, 'secs': secs + secs2,
                                 'tail': (tail if rc else '') + (tail2 if rc2 else '')}
        gates['signer_equiv'] = {'pass': rc == 0 and runs >= 4, 'mode': 'hermetic-no-cli',
                                 'rows': [{'cli_version': args.cli_version, 'signer': 'signAnthropicMessagesBody over the sent body', 'signer_equal': rc == 0}],
                                 'tail': tail if rc else ''}
    elif not have_cli:
        gates['wire_capture'] = dict(missing)
    elif args.skip_wire:
        gates['wire_capture'] = {'pass': True, 'skipped': True}
    elif not gates['build']['pass']:
        gates['wire_capture'] = {'pass': False, 'reason': 'build failed'}
    else:
        rep = out / 'wire-capture.json'
        rc, tail, secs = run([sys.executable, str(HERE / 'cch_capture.py'), '--harness', str(out / 'dpx-alias-offline'),
                              '--claude', str(cli), '--cch-js', str(args.cch_js), '--report', str(rep)], timeout=300)
        rows = json.loads(rep.read_text())['requests'] if rep.exists() else []
        want_ua = 'claude-cli/%s (external, cli)' % args.cli_version
        main_rows = [r for r in rows if r.get('billing_block')]
        checks = {
            'main_loop_seen': rc == 0,
            'brand_leak_zero': bool(rows) and not any(r['brand_in_body'] for r in rows),
            'ua_preserved': bool(main_rows) and all(r['upstream_user_agent'] == want_ua for r in main_rows),
            'entrypoint_cli': bool(main_rows) and all(r['cc_entrypoint_cli'] for r in main_rows),
            'cch_valid': bool(main_rows) and all(r['cch_valid'] for r in main_rows),
        }
        gates['wire_capture'] = {'pass': all(checks.values()), 'checks': checks, 'requests': len(rows), 'secs': secs,
                                 'tail': '' if rc == 0 else tail}
    # genuine self-signed bodies -> CPA signer + detector; model catalog runs regardless
    caps = out / 'cli-sign-captures.json'
    rc1, tail1 = 0, ''
    if have_cli:
        rc1, tail1, _ = run([sys.executable, str(HERE / 'cli_sign_capture.py'), '--cli', str(cli), '--out', str(caps)], timeout=300)
    probe = out / 'coupling-probe.json'
    penv = dict(env, DPX_COUPLING_REPORT=str(probe), DPX_COUPLING_MODELS=args.models, DPX_COUPLING_WINDOW_EXTRA=args.cli_version)
    if have_cli and caps.exists():
        penv['DPX_COUPLING_CAPTURES'] = str(caps)
    rc2, tail2, _ = run(['go', 'test', './internal/runtime/executor', '-run', 'TestDPXCouplingProbe', '-count=1'], cwd=src, env=penv)
    rep = json.loads(probe.read_text()) if probe.exists() else {}
    rows = rep.get('captures') or []
    signed = [r for r in rows if r.get('cli_cch')]
    if args.no_cli:
        window = {r['version']: r.get('plausible_stock') for r in rep.get('version_window') or []}
        gates['cli_window'] = {'pass': rc2 == 0 and bool(window.get(args.cli_version)) and not window.get('3.0.0', True),
                               'mode': 'hermetic-no-cli', 'cpa_baseline_ua': rep.get('baseline_user_agent'),
                               'rows': [{'cli_version': args.cli_version, 'plausible_stock': window.get(args.cli_version)}],
                               'tail': tail2 if rc2 else ''}
    elif not have_cli:
        gates['signer_equiv'] = dict(missing)
        gates['cli_window'] = dict(missing)
    else:
        gates['signer_equiv'] = {'pass': rc1 == 0 and rc2 == 0 and bool(signed) and all(r.get('signer_equal') for r in signed),
                                 'rows': [{k: r.get(k) for k in ('cli_version', 'cli_cch', 'cpa_cch', 'signer_equal')} for r in signed],
                                 'tail': (tail1 if rc1 else '') + (tail2 if rc2 else '')}
        gates['cli_window'] = {'pass': bool(rows) and all(r.get('detector_confirmed') for r in rows),
                               'cpa_baseline_ua': rep.get('baseline_user_agent'),
                               'rows': [{k: r.get(k) for k in ('cli_version', 'detector_confirmed', 'ua_preserved')} for r in rows]}
    if args.models:
        cat = rep.get('model_catalog') or {}
        gates['model_catalog'] = {'pass': rc2 == 0 and bool(cat) and all(cat.values()),
                                  'missing': sorted(k for k, v in cat.items() if not v), 'checked': len(cat),
                                  'tail': tail2 if rc2 else ''}

    sha = run(['git', 'rev-parse', 'HEAD'], cwd=src)[1].strip()
    summary = {'source': str(src), 'sha': sha, 'cli_version': args.cli_version, 'cli_present': have_cli, 'gates': gates,
               'failed': sorted(k for k, v in gates.items() if not v['pass'])}
    summary['green'] = not summary['failed']
    (out / 'summary.json').write_text(json.dumps(summary, indent=2) + '\n')
    print(json.dumps({'green': summary['green'], 'failed': summary['failed'], 'cli_present': have_cli}))
    return 0 if summary['green'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
