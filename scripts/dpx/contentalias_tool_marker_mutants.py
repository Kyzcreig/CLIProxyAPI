#!/usr/bin/env python3
"""Mutation check for the near-copied tool alias marker (t_7bd8c9b9).

Each mutant reverts or weakens one piece of the unknown_tool_N marker, runs
the regression tests in tool_alias_near_copy_test.go, and restores the source
with `git checkout`. A mutant that no test catches is a gap: the script exits 1.

Run from the repo root on a host that may run `go test` (CI or ACE-AI):
    python3 scripts/dpx/contentalias_tool_marker_mutants.py
"""
import re
import subprocess
import sys
from pathlib import Path

PKG = Path("internal/contentalias")
TESTS = ["TestToolAliasNearCopyStreamBecomesMarker",
         "TestToolAliasNearCopyJSONBecomesMarker",
         "TestToolAliasExactStillRestores",
         "TestToolNameNotAliasShapedStillRefused",
         "TestToolMarkerHistoryPreparesCleanly"]


def sub(pattern, repl, flags=0):
    def f(src):
        out, n = re.subn(pattern, repl, src, count=1, flags=flags)
        if n != 1:
            raise SystemExit(f"mutant anchor not found: {pattern!r}")
        return out
    return f


MUTANTS = [
    # The pre-fix stream: any undeclared name refuses the turn.
    ("stream_marker_reverted", "stream.go",
     sub(r"\t\t\t\tif !strayClassPattern\.MatchString\(name\) \{\n", "\t\t\t\tif true {\n")),
    # The pre-fix non-stream restore: any undeclared name refuses the turn.
    ("json_marker_reverted", "request.go",
     sub(r"\t\t\tif !strayClassPattern\.MatchString\(name\.str\(\)\) \{\n", "\t\t\tif true {\n")),
    # Marker for every unknown name, alias-shaped or not.
    ("stream_marker_for_raw_names", "stream.go",
     sub(r"\t\t\t\tif !strayClassPattern\.MatchString\(name\) \{\n", "\t\t\t\tif false {\n")),
    ("json_marker_for_raw_names", "request.go",
     sub(r"\t\t\tif !strayClassPattern\.MatchString\(name\.str\(\)\) \{\n", "\t\t\tif false {\n")),
    # Marker keeps the model's input instead of {}.
    ("json_marker_keeps_input", "request.go",
     sub(r'\t\t\t\t\*edits = append\(\*edits, edit\{input\.start, input\.end, \[\]byte\("\{\}"\)\}\)\n', "")),
    # Marker that still carries an alias token (would be refused as history_tool next turn).
    ("marker_alias_shaped", "toolmarker.go",
     sub(r'return "unknown_tool_" \+', 'return "dpx_v1_t_unknown_" +')),
]


def run_tests():
    p = subprocess.run(["go", "test", "./" + str(PKG), "-run", "^(" + "|".join(TESTS) + ")$",
                        "-count=1", "-v"], capture_output=True, text=True)
    out = p.stdout + p.stderr
    status = {t: ("RED" if f"--- FAIL: {t}" in out else "green" if f"--- PASS: {t}" in out else "no-run") for t in TESTS}
    return status, out


def main():
    base, out = run_tests()
    if any(v != "green" for v in base.values()):
        print(out)
        sys.exit(f"baseline not green: {base}")
    rows, gaps = [], []
    for name, fname, mutate in MUTANTS:
        path = PKG / fname
        src = path.read_text()
        try:
            path.write_text(mutate(src))
            status, out = run_tests()
        finally:
            subprocess.run(["git", "checkout", "--", str(path)], check=True)
        if "build failed" in out or "[setup failed]" in out:
            print(out)
            sys.exit(f"mutant {name} did not compile")
        caught = [t for t, v in status.items() if v == "RED"]
        rows.append((name, caught))
        if not caught:
            gaps.append(name)
    w = max(len(n) for n, _ in rows)
    for name, caught in rows:
        print(f"{name:<{w}}  RED: {', '.join(caught) or '-'}")
    if gaps:
        sys.exit(f"uncaught mutants: {gaps}")


if __name__ == "__main__":
    main()
