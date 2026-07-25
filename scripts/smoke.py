"""End-to-end smoke test against a running compose stack.

Walks the whole pipeline the README describes — upload, extract, review, draft,
commit, snapshot, backtest, candidates — and asserts the final ranking is dense
and carries page-cited evidence. This is the check CI cannot run: it needs the
stack up and network access for market data.

    docker compose up --build -d
    python scripts/smoke.py

Everything it creates is additive: documents dedupe on their SHA-256, and the
strategy slug is suffixed so repeated runs never collide.
"""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path
from typing import Any, Callable

API = "http://localhost:8080"
SAMPLES = Path(__file__).resolve().parent.parent / "samples" / "research"
PUBLISHED_AT = "2026-01-05T00:00:00Z"
CUTOFF_AT = "2026-03-01T00:00:00Z"
START_DATE, END_DATE = "2024-01-02", "2026-03-02"


class SmokeError(RuntimeError):
    """The pipeline did not behave as documented."""


def call(method: str, path: str, body: Any = None, headers: dict[str, str] | None = None):
    data = None
    headers = dict(headers or {})
    if isinstance(body, (bytes, bytearray)):
        data = body
    elif body is not None:
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
        raise SmokeError(f"{what}: expected {wanted}, got {status} {body}")
    return body


def multipart(path: Path) -> tuple[bytes, dict[str, str]]:
    boundary = uuid.uuid4().hex
    payload = b"".join([
        f'--{boundary}\r\nContent-Disposition: form-data; name="published_at"\r\n\r\n{PUBLISHED_AT}\r\n'.encode(),
        f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{path.name}"\r\n'
        f"Content-Type: application/pdf\r\n\r\n".encode(),
        path.read_bytes(),
        f"\r\n--{boundary}--\r\n".encode(),
    ])
    return payload, {"Content-Type": f"multipart/form-data; boundary={boundary}"}


def poll(path: str, done: Callable[[Any], bool], what: str, attempts: int = 120):
    status, body = None, None
    for _ in range(attempts):
        status, body = call("GET", path)
        if status == 200 and done(body):
            return body
        time.sleep(2)
    raise SmokeError(f"timed out waiting for {what}: last={status} {body}")


def step(message: str):
    print(f"\n== {message}", flush=True)


def main() -> None:
    step("health")
    print(expect(call("GET", "/healthz"), 200, "healthz"))

    step("upload the sample research PDFs")
    document_ids = []
    for sample in sorted(SAMPLES.glob("*.pdf")):
        status, document = call("POST", "/v1/documents", *multipart(sample))
        if status not in (200, 202):
            raise SmokeError(f"upload {sample.name}: {status} {document}")
        document_ids.append(document["id"])
        print(f"  {'duplicate' if document['duplicate'] else 'new      '} {sample.name[:60]}")
    if not document_ids:
        raise SmokeError(f"no sample PDFs found under {SAMPLES}")

    step("wait for extraction")
    for document_id in document_ids:
        document = poll(
            f"/v1/documents/{document_id}",
            lambda body: body["status"] in {"ready", "failed"},
            f"extraction of {document_id}",
        )
        if document["status"] != "ready":
            raise SmokeError(f"extraction failed for {document_id}")
    print(f"  {len(document_ids)} documents ready")

    step("review claims: accept the positive ones")
    claims = []
    for document_id in document_ids:
        _, body = call("GET", f"/v1/documents/{document_id}/claims")
        claims.extend(body["claims"])
    positive = [claim for claim in claims if claim["direction"] == "positive"][:5]
    if not positive:
        raise SmokeError(f"none of the {len(claims)} extracted claims were positive; nothing to draft from")

    accepted = []
    for claim in positive:
        _, reviewed = call("PATCH", f"/v1/claims/{claim['id']}", {"validation_status": "accepted"})
        accepted.append(reviewed["id"])
        print(f"  p{reviewed['page_number']:<4} {reviewed['claim'][:70]}")

    step("draft a strategy from the accepted claims")
    draft = expect(call("POST", "/v1/strategies/draft", {"claim_ids": accepted}), 200, "draft")
    spec = draft["spec"]
    print(f"  {spec['selection']['template']} over {spec['universe']['symbols']}")

    step("commit the draft as an immutable version")
    spec["slug"] = f"{spec['slug'][:50]}-{uuid.uuid4().hex[:6]}"
    strategy = expect(call("POST", "/v1/strategies", {"spec": spec, "claim_ids": accepted}), 201, "commit")
    strategy_id = strategy["id"]
    print(f"  {strategy['slug']} v{strategy['version']}")

    step("snapshot the market data the universe needs")
    expect(call("POST", "/v1/market-data/snapshots", {
        "symbols": spec["universe"]["symbols"],
        "start_date": START_DATE,
        "end_date": END_DATE,
    }), 202, "snapshot request")

    def is_ours(item: dict[str, Any]) -> bool:
        return bool(
            item.get("checksum")
            and item["start_date"] == START_DATE
            and item["end_date"] == END_DATE
            and item["symbols"] == spec["universe"]["symbols"]
        )

    snapshots = poll("/v1/market-data/snapshots", lambda body: any(map(is_ours, body["snapshots"])), "snapshot")
    snapshot = next(item for item in snapshots["snapshots"] if is_ours(item))
    print(f"  {snapshot['symbols']} {snapshot['start_date']}..{snapshot['end_date']} sha={snapshot['checksum'][:12]}")

    step("run a backtest")
    run = expect(call("POST", "/v1/backtests", {
        "strategy_id": strategy_id,
        "market_data_snapshot_id": snapshot["id"],
        "document_cutoff_at": CUTOFF_AT,
    }), 202, "backtest request")
    run_id = run["id"]

    run = poll(f"/v1/backtests/{run_id}", lambda body: body["status"] in {"completed", "failed"}, "backtest")
    if run["status"] != "completed":
        raise SmokeError(f"backtest failed: {run}")
    summary = run["summary"]
    print(f"  {run['engine_version']} sha={run['result_checksum'][:12]}")
    print(f"  {summary['n_rebalances']} rebalances over {summary['n_trading_days']} days, "
          f"{summary['n_claim_signals']} claim signals live in "
          f"{summary['n_claim_supported_rebalances']} of them")
    # The accounting the numbers came out of. invested_fraction in particular is
    # what tells a flat curve from a book that was mostly sitting in cash.
    print(f"  {summary['position_tracking']} positions, {summary['momentum_scale']}-space momentum, "
          f"fills +{summary['execution_lag_days']}d, cash_policy={summary['cash_policy']}")
    print(f"  turnover {summary['total_turnover']:.2f}, "
          f"invested {summary['invested_fraction']:.1%}, "
          f"{summary['n_stop_loss_exits']} stop-loss exits")
    if summary["n_rebalances"] < 1:
        raise SmokeError("the run never rebalanced; widen the snapshot or shorten lookback_months")
    if summary["invested_fraction"] <= 0:
        raise SmokeError("the run never held anything; check max_position_weight against top_n")

    step("candidates for this run")
    body = expect(call("GET", f"/v1/candidates?backtest_id={run_id}"), 200, "candidates")
    candidates = body["candidates"]
    if not candidates:
        raise SmokeError("a completed run produced no candidates")
    if [item["rank"] for item in candidates] != list(range(1, len(candidates) + 1)):
        raise SmokeError(f"ranks are not dense: {[item['rank'] for item in candidates]}")
    if not any(item["evidence"] for item in candidates):
        raise SmokeError("no candidate carried evidence")

    weight = summary["claim_signal_weight"]
    for candidate in candidates:
        # The documented formula, verifiable from the response alone. The engine
        # scores the cross-sectional rank of momentum rather than the raw
        # trailing return, so `momentum_rank` is the term that enters the sum and
        # `momentum` stays the return a reviewer can read off a price chart.
        ranked = candidate.get("momentum_rank")
        if ranked is None:
            raise SmokeError(f"{candidate['symbol']} is missing momentum_rank")
        expected = ranked + weight * candidate["claim_support"]
        if abs(candidate["score"] - expected) > 1e-6:
            raise SmokeError(f"{candidate['symbol']} score {candidate['score']} != {expected}")
        print(f"  #{candidate['rank']} {candidate['symbol']:<6} weight={candidate['weight']:.3f} "
              f"score={candidate['score']:.4f} = rank {ranked:+.4f} "
              f"+ {weight} x support {candidate['claim_support']:.2f} "
              f"(momentum {candidate['momentum']:+.4f})")
        for item in candidate["evidence"]:
            if not item["page_number"] or not item["evidence_quote"]:
                raise SmokeError(f"{candidate['symbol']} cites a claim with no page evidence")
            print(f"       p{item['page_number']} ({item['contribution']:+.2f}) {item['claim'][:66]}")

    step("candidates for the strategy (its latest completed run)")
    latest = expect(call("GET", f"/v1/candidates?strategy_id={strategy_id}"), 200, "candidates by strategy")
    print(f"  {[(item['symbol'], item['rank'], item['as_of']) for item in latest['candidates']]}")

    print("\nSMOKE TEST PASSED", flush=True)


if __name__ == "__main__":
    try:
        main()
    except SmokeError as error:
        raise SystemExit(f"\nSMOKE TEST FAILED: {error}")
