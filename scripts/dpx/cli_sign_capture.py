"""Capture what a GENUINE Claude Code binary signs by itself (no CPA in the path).

For each CLI binary given, run `claude -p` under sandbox-exec (network limited to
localhost) against a loopback recorder with the first-party flag set, so the CLI's
own signer writes `cch`. A synthetic OAuth-shaped token is used; nothing leaves the
host. The output feeds dpx_coupling_probe_test.go (DPX_COUPLING_CAPTURES), which
re-signs each body with CPA's signer and runs CPA's native-client detector on the
recorded headers.

Raw bodies stay under a private 0700 work dir. The printed/returned JSON carries
bodies only in the captures file the Go probe reads (also inside the work dir).
"""
import argparse
import http.server
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading

SANDBOX = '(version 1) (allow default) (deny network*) (allow network-inbound (local ip "localhost:*")) (allow network-outbound (remote ip "localhost:*")) (allow network-bind (local ip "localhost:*"))'
FAKE_OAUTH = 'sk-ant-oat01-' + 'x' * 86 + '-labAA'


def sse(model):
    events = [
        {'type': 'message_start', 'message': {'id': 'offline', 'type': 'message', 'role': 'assistant', 'content': [], 'model': model, 'stop_reason': None, 'stop_sequence': None, 'usage': {'input_tokens': 5, 'output_tokens': 1}}},
        {'type': 'content_block_start', 'index': 0, 'content_block': {'type': 'text', 'text': ''}},
        {'type': 'content_block_delta', 'index': 0, 'delta': {'type': 'text_delta', 'text': 'ok'}},
        {'type': 'content_block_stop', 'index': 0},
        {'type': 'message_delta', 'delta': {'stop_reason': 'end_turn', 'stop_sequence': None}, 'usage': {'output_tokens': 1}},
        {'type': 'message_stop'},
    ]
    return b''.join(('event: %s\ndata: %s\n\n' % (e['type'], json.dumps(e))).encode() for e in events)


def capture(cli, work, model, prompt):
    rows = []

    class Rec(http.server.BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_POST(self):
            body = self.rfile.read(int(self.headers.get('Content-Length', 0) or 0))
            if self.path.split('?')[0] == '/v1/messages':
                rows.append({'path': self.path, 'headers': {k: self.headers.get_all(k) for k in self.headers.keys()},
                             'body': body.decode('utf-8', 'surrogateescape')})
            if self.path.startswith('/v1/messages/count_tokens'):
                out, ctype = b'{"input_tokens":1}', 'application/json'
            elif body and json.loads(body).get('stream'):
                out, ctype = sse(json.loads(body).get('model', 'm')), 'text/event-stream'
            elif self.path.startswith('/v1/messages'):
                out, ctype = json.dumps({'id': 'offline', 'type': 'message', 'role': 'assistant', 'model': model, 'content': [{'type': 'text', 'text': 'ok'}], 'stop_reason': 'end_turn', 'usage': {'input_tokens': 5, 'output_tokens': 1}}).encode(), 'application/json'
            else:
                out, ctype = b'{}', 'application/json'
            self.send_response(200)
            self.send_header('Content-Type', ctype)
            self.send_header('Content-Length', str(len(out)))
            self.end_headers()
            self.wfile.write(out)

        do_GET = do_POST

    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Rec)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    home = Path(tempfile.mkdtemp(dir=work))
    cfg = home / '.claude'
    cfg.mkdir()
    proj = home / 'proj'
    proj.mkdir()
    (cfg / '.claude.json').write_text(json.dumps({'hasCompletedOnboarding': True, 'theme': 'dark', 'projects': {str(proj): {'hasTrustDialogAccepted': True}}}))
    env = {k: v for k, v in os.environ.items() if not k.startswith(('ANTHROPIC_', 'CLAUDE_', '_CLAUDE_'))}
    env.update(HOME=str(home), CLAUDE_CONFIG_DIR=str(cfg), ANTHROPIC_BASE_URL='http://127.0.0.1:%d' % server.server_port,
               CLAUDE_CODE_OAUTH_TOKEN=FAKE_OAUTH, _CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL='1',
               CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC='1', CLAUDE_CODE_MAX_RETRIES='0')
    try:
        rc = subprocess.run(['/usr/bin/sandbox-exec', '-p', SANDBOX, str(cli), '-p', '--model', model, prompt],
                            cwd=proj, env=env, capture_output=True, timeout=90).returncode
    except subprocess.TimeoutExpired:
        rc = 124
    server.shutdown()
    return rc, rows


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--cli', type=Path, action='append', required=True, help='claude binary; repeatable')
    ap.add_argument('--out', type=Path, required=True, help='captures JSON for the Go probe (private)')
    ap.add_argument('--model', default='claude-haiku-4-5-20251001')
    ap.add_argument('--prompt', default='Say ok.')
    args = ap.parse_args()
    work = Path(tempfile.mkdtemp(prefix='dpx-sign-')).resolve()  # realpath: the CLI keys folder trust on the resolved cwd; /tmp -> /private/tmp under launchd (no TMPDIR) otherwise stalls on the trust dialog
    os.chmod(work, 0o700)
    out = []
    for cli in args.cli:
        version = subprocess.run([str(cli), '--version'], capture_output=True, text=True).stdout.split(' ')[0].strip()
        rc, rows = capture(cli, work, args.model, args.prompt)
        for r in rows:
            r.update(cli_version=version, cli_rc=rc)
        out.extend(rows)
        print(json.dumps({'cli_version': version, 'rc': rc, 'messages_requests': len(rows)}))
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(out))
    os.chmod(args.out, 0o600)
    return 0 if out else 1


if __name__ == '__main__':
    raise SystemExit(main())
