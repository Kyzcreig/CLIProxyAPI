"""Offline loopback capture: stock Claude Code TUI -> dpx-alias-offline (real CPA
executor, alias ON, synthetic OAuth-shaped upstream key so CPA's native CCH
signing runs) -> scripted loopback upstream that records every request.

No provider egress: the CLI and harness run under sandbox-exec with network
limited to localhost. The CLI runs stock: CLAUDE_CODE_ATTRIBUTION_HEADER is
removed from its environment. Raw bodies stay under the private work dir; the
report holds only derived fields (block text, cch, UA, validity, brand check).
"""
import argparse
import http.server
import json
import os
from pathlib import Path
import pty
import secrets
import select
import signal
import subprocess
import tempfile
import threading
import time
import uuid

SANDBOX = '(version 1) (allow default) (deny network*) (allow network-inbound (local ip "localhost:*")) (allow network-outbound (remote ip "localhost:*")) (allow network-bind (local ip "localhost:*"))'
BRANDS = (b'hermes', b'openclaw')
CCH_JS = 'const {computeCch}=require(process.argv[1]);const fs=require("fs");const b=fs.readFileSync(0);const s=b.toString("utf8");const m=s.match(/cch=([0-9a-f]{5});/);if(!m){console.log("none");process.exit(0)}console.log(m[1]+" "+computeCch(Buffer.from(s.replace("cch="+m[1]+";","cch=00000;"),"utf8")))'


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


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--harness', type=Path, required=True)
    ap.add_argument('--claude', type=Path, required=True)
    ap.add_argument('--cch-js', type=Path, required=True)
    ap.add_argument('--report', type=Path, required=True)
    ap.add_argument('--prompt', default='Say ok. Hermes sandbox.')
    args = ap.parse_args()
    work = Path(tempfile.mkdtemp(prefix='dpx-cch-')).resolve()  # realpath: the CLI keys folder trust on the resolved cwd; /tmp -> /private/tmp under launchd (no TMPDIR) otherwise stalls on the trust dialog
    os.chmod(work, 0o700)
    captures = []

    class Upstream(http.server.BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_POST(self):
            body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
            path = work / ('req-%02d.json' % len(captures))
            path.write_bytes(body)
            captures.append({'path': self.path, 'ua': self.headers.get('User-Agent', ''), 'file': str(path)})
            if self.path.startswith('/v1/messages/count_tokens'):
                out, ctype = b'{"input_tokens":1}', 'application/json'
            elif json.loads(body or b'{}').get('stream'):
                out, ctype = sse(json.loads(body).get('model', 'm')), 'text/event-stream'
            else:
                out, ctype = json.dumps({'id': 'offline', 'type': 'message', 'role': 'assistant', 'model': 'm', 'content': [{'type': 'text', 'text': 'ok'}], 'stop_reason': 'end_turn', 'usage': {'input_tokens': 5, 'output_tokens': 1}}).encode(), 'application/json'
            self.send_response(200)
            self.send_header('Content-Type', ctype)
            self.send_header('Content-Length', str(len(out)))
            self.end_headers()
            self.wfile.write(out)

        do_GET = do_POST

    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    store = work / 'store'
    store.mkdir(mode=0o700)
    client_key = secrets.token_hex(16)
    config = work / 'harness.json'
    config.write_text(json.dumps({'Upstream': 'http://127.0.0.1:%d' % server.server_port, 'ClientKey': client_key, 'StoreDirectory': str(store), 'SessionID': str(uuid.uuid4()), 'Initialize': True, 'UpstreamKey': 'sk-ant-oat01-offline-synthetic'}))
    harness = subprocess.Popen(['/usr/bin/sandbox-exec', '-p', SANDBOX, str(args.harness), '--config', str(config)], stdout=subprocess.PIPE, stderr=open(work / 'harness.log', 'w'), text=True)
    address = harness.stdout.readline().strip()
    home = work / 'home'
    cfgdir = home / '.claude'
    cfgdir.mkdir(parents=True)
    proj = work / 'proj'
    proj.mkdir()
    (cfgdir / '.claude.json').write_text(json.dumps({'hasCompletedOnboarding': True, 'theme': 'dark', 'projects': {str(proj): {'hasTrustDialogAccepted': True, 'hasCompletedProjectOnboarding': True}}}))
    env = {k: v for k, v in os.environ.items() if not k.startswith(('ANTHROPIC_', 'CLAUDE_'))}
    env.update(HOME=str(home), CLAUDE_CONFIG_DIR=str(cfgdir), ANTHROPIC_BASE_URL='http://' + address, ANTHROPIC_AUTH_TOKEN=client_key, TERM='xterm-256color', CLAUDE_CODE_MAX_RETRIES='0')
    assert 'CLAUDE_CODE_ATTRIBUTION_HEADER' not in env
    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(proj)
        os.execve('/usr/bin/sandbox-exec', ['/usr/bin/sandbox-exec', '-p', SANDBOX, str(args.claude), '--model', 'claude-haiku-4-5', args.prompt], env)
    screen = b''
    deadline = time.time() + 90
    main_seen = False
    while time.time() < deadline and not main_seen:
        r, _, _ = select.select([fd], [], [], 0.5)
        if r:
            try:
                screen += os.read(fd, 65536)
            except OSError:
                break
        for c in captures:
            raw = Path(c['file']).read_bytes()
            if c['path'].split('?')[0] == '/v1/messages' and b'x-anthropic-billing-header' in raw and b'"tools"' in raw:
                main_seen = True
        if b'Enter to confirm' in screen[-4000:] or b'trust' in screen[-4000:].lower():
            os.write(fd, b'\r')
            screen += b'[sent-enter]'
    time.sleep(2)
    os.kill(pid, signal.SIGKILL)
    harness.kill()
    server.shutdown()
    (work / 'screen.bin').write_bytes(screen)
    rows = []
    for c in captures:
        raw = Path(c['file']).read_bytes()
        body = json.loads(raw) if raw.strip() else {}
        system = body.get('system')
        block = ''
        if isinstance(system, list) and system and isinstance(system[0], dict):
            text = system[0].get('text', '')
            if text.startswith('x-anthropic-billing-header:'):
                block = text
        check = subprocess.run(['node', '-e', CCH_JS, str(args.cch_js)], input=raw, capture_output=True).stdout.decode().split()
        rows.append({
            'path': c['path'], 'upstream_user_agent': c['ua'], 'model': body.get('model'), 'stream': body.get('stream'),
            'tools': len(body.get('tools') or []), 'billing_block': block,
            'cc_entrypoint_cli': 'cc_entrypoint=cli;' in block,
            'cch_sent': check[0] if check and check[0] != 'none' else None,
            'cch_recomputed_over_sent_body': check[1] if len(check) > 1 else None,
            'cch_valid': len(check) > 1 and check[0] == check[1],
            'brand_in_body': any(b in raw.lower() for b in BRANDS), 'body_len': len(raw),
        })
    report = {'work_dir_private': str(work), 'cli': str(args.claude), 'attribution_header_env': None,
              'harness_address': address, 'main_loop_request_seen': main_seen, 'requests': rows,
              'harness_log_tail': (work / 'harness.log').read_text()[-600:]}
    args.report.write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report, indent=2))
    return 0 if main_seen else 1


if __name__ == '__main__':
    raise SystemExit(main())
