"""Re-run a completed backtest under both engine versions and compare them.

ADR 0008 keeps momentum-claims-v3 reachable so a strategy that ran under it can
be re-run and diffed before the frozen simulator is deleted.

    python scripts/engine_diff.py [backtest_id]

With no argument it picks the newest completed run. Both runs reuse that run's
strategy, snapshot and cutoff, so the only difference is the accounting.
"""

from __future__ import annotations

import json
import sys
import time
import urllib.error
import urllib.request
from typing import Any

API = "http://localhost:8080"

V4 = "momentum-claims-v4"
V3 = "momentum-claims-v3"

METRICS = [
    ("total_return", "{:+.4%}"),
    ("cagr", "{:+.4%}"),
    ("sharpe", "{:+.3f}"),
    ("annualized_volatility", "{:.4%}"),
    ("max_drawdown", "{:+.4%}"),
    ("total_turnover", "{:.4f}"),
    ("invested_fraction", "{:.2%}"),
    ("n_rebalances", "{}"),
    ("n_stop_loss_exits", "{}"),
]


class DiffError(RuntimeError):
    pass


def call(method: str, path: str, body: Any = None):
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(API + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=120) as response:
            raw = response.read()
            return response.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as error:
        raw = error.read()
        try:
            return error.code, json.loads(raw)
        except json.JSONDecodeError:
            return error.code, raw.decode(errors="replace")


def expect(result: tuple[int, Any], wanted: int, what: str):
    status, body = result
    if status != wanted:
        raise DiffError(f"{what}: expected {wanted}, got {status} {body}")
    return body


def newest_run() -> str:
    body = expect(call("GET", "/v1/candidates"), 200, "candidates")
    if not body["candidates"]:
        raise DiffError("no completed runs to diff; run scripts/smoke.py first")
    return body["candidates"][0]["backtest_run_id"]


def run_under(source: dict, engine_version: str) -> dict:
    parameters = dict(source.get("parameters") or {})
    parameters["engine_version"] = engine_version

    created = expect(call("POST", "/v1/backtests", {
        "strategy_id": source["strategy_id"],
        "market_data_snapshot_id": source["market_data_snapshot_id"],
        "document_cutoff_at": source["document_cutoff_at"],
        "parameters": parameters,
    }), 202, f"create {engine_version} run")

    for _ in range(120):
        status, body = call("GET", f"/v1/backtests/{created['id']}")
        if status == 200 and body["status"] in {"completed", "failed"}:
            if body["status"] == "failed":
                raise DiffError(f"{engine_version} run failed: {body}")
            return body
        time.sleep(2)
    raise DiffError(f"timed out waiting for the {engine_version} run")


def show(label: str, value: Any, template: str) -> str:
    if value is None:
        return "-"
    try:
        return template.format(value)
    except (ValueError, TypeError):
        return str(value)


def main() -> None:
    source_id = sys.argv[1] if len(sys.argv) > 1 else newest_run()
    source = expect(call("GET", f"/v1/backtests/{source_id}"), 200, "source run")
    print(f"source run {source_id} ({source.get('engine_version')})")
    print(f"  strategy {source['strategy_id']}")
    print(f"  snapshot {source['market_data_snapshot_id']}")
    print(f"  cutoff   {source['document_cutoff_at']}")

    current = run_under(source, V4)
    previous = run_under(source, V3)

    print(f"\n{'':<24}{V4:>20}{V3:>20}")
    print("-" * 64)
    for name, template in METRICS:
        left = show(name, current["summary"].get(name), template)
        right = show(name, previous["summary"].get(name), template)
        marker = "" if left == right else "  <-"
        print(f"{name:<24}{left:>20}{right:>20}{marker}")

    print(f"{'result_checksum':<24}{current['result_checksum'][:16]:>20}{previous['result_checksum'][:16]:>20}")

    if current["result_checksum"] == previous["result_checksum"]:
        raise DiffError("both engines produced the same curve; the version bump changed nothing")

    turnover = current["summary"]["total_turnover"] - previous["summary"]["total_turnover"]
    ret = current["summary"]["total_return"] - previous["summary"]["total_return"]
    print(f"\nv4 charges {turnover:+.4f} more turnover and reports {ret:+.4%} return")
    print(f"v4 held {current['summary']['invested_fraction']:.1%} of the book on average")


if __name__ == "__main__":
    try:
        main()
    except DiffError as error:
        raise SystemExit(f"\nENGINE DIFF FAILED: {error}")
