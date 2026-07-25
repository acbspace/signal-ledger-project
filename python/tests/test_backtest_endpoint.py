"""HTTP-level tests for the backtest endpoint.

test_backtest.py calls the engine directly and never touches the response model.
Pydantic drops any field CandidatePosition does not declare, without raising, so
a missing field reaches Go as a zero and gets persisted as one. These tests cover
that gap.
"""

import datetime as dt
import hashlib

from fastapi.testclient import TestClient

from quant_service import backtest
from quant_service.main import app

SPEC = {
    "slug": "momentum-us",
    "version": 1,
    "name": "US Momentum",
    "universe": {"name": "U", "asset_class": "equity", "symbols": ["AAA", "BBB", "CCC"]},
    "selection": {
        "template": "research-supported-momentum",
        "filters": [
            {"field": "lookback_months", "operator": "eq", "value": 1},
            {"field": "top_n", "operator": "eq", "value": 2},
        ],
    },
    "rebalance": {"schedule": "monthly"},
    "risk": {"max_position_weight": 0.6, "transaction_cost_bps": 10},
}


def _snapshot(tmp_path) -> tuple[str, str]:
    """Freeze a canonical snapshot CSV and return its storage key and checksum."""

    days: list[dt.date] = []
    day = dt.date(2020, 1, 1)
    while len(days) < 30:
        if day.weekday() < 5:
            days.append(day)
        day += dt.timedelta(days=1)

    lines = ["symbol,date,open,high,low,close,adj_close,volume"]
    for symbol, step in (("AAA", 1.0), ("BBB", 0.5), ("CCC", -0.3)):
        for index, current in enumerate(days):
            price = 100.0 + step * index
            lines.append(
                f"{symbol},{current.isoformat()},{price},{price},{price},{price},{price},100"
            )
    raw = ("\n".join(lines) + "\n").encode("utf-8")

    storage_key = "market-data/snapshot.csv"
    path = tmp_path / storage_key
    path.parent.mkdir(parents=True)
    path.write_bytes(raw)
    return storage_key, hashlib.sha256(raw).hexdigest()


def _run(tmp_path, monkeypatch, **overrides) -> dict:
    storage_key, checksum = _snapshot(tmp_path)
    monkeypatch.setenv("DOCUMENT_STORAGE_PATH", str(tmp_path))

    body = {
        "backtest_id": "0a5f6f16-0a1e-4f0a-9d0e-2f2b4a0f6f11",
        "spec": SPEC,
        "snapshot_storage_key": storage_key,
        "snapshot_checksum": checksum,
        "document_cutoff_at": "2020-03-01T00:00:00Z",
        # AAA ranks +1 and CCC -1, so a weight above 2.0 is what carries a
        # full-confidence claim on CCC clear of AAA rather than into a tie.
        "parameters": {"claim_signal_weight": 3.0},
        "signals": [
            {
                "claim_id": "claim-1",
                "symbol": "CCC",
                "direction": "positive",
                "confidence": 1.0,
                "effective_at": "2020-01-01T00:00:00Z",
                "horizon_days": 400,
            }
        ],
    }
    body.update(overrides)

    response = TestClient(app).post("/v1/backtests", json=body)
    assert response.status_code == 200, response.text
    return response.json()


def test_candidate_response_carries_every_term_of_its_score(tmp_path, monkeypatch) -> None:
    payload = _run(tmp_path, monkeypatch)

    positions = payload["candidates"]["positions"]
    assert positions, payload["candidates"]

    weight = payload["summary"]["claim_signal_weight"]
    for position in positions:
        # momentum_rank was absent from the response model and arrived as 0 for
        # every position. A zero looks like a mid-ranked name, so nothing failed.
        assert "momentum_rank" in position, position
        assert position["score"] == round(
            position["momentum_rank"] + weight * position["claim_support"], 8
        ), position

    # The tilt puts the worst momentum name on top, so dropping the rank term
    # reverses the ranking rather than shifting a decimal.
    assert positions[0]["symbol"] == "CCC"
    assert positions[0]["momentum_rank"] == -1.0
    assert positions[0]["momentum"] < 0


def test_summary_reports_the_accounting_over_the_wire(tmp_path, monkeypatch) -> None:
    payload = _run(tmp_path, monkeypatch)

    # summary is a free-form dict, so no schema protects these names. A stored
    # run explains its own numbers with them.
    summary = payload["summary"]
    for field in (
        "position_tracking",
        "momentum_scale",
        "execution_lag_days",
        "cash_policy",
        "invested_fraction",
        "n_stop_loss_exits",
        "python_version",
        "polars_version",
    ):
        assert field in summary, sorted(summary)

    assert payload["engine_version"] == backtest.ENGINE_VERSION
    assert len(payload["checksum"]) == 64
    assert payload["equity_curve_csv"].startswith("date,equity\n")


def test_previous_engine_is_reachable_over_the_wire(tmp_path, monkeypatch) -> None:
    # The v3/v4 diff has to work through the API, not only in a unit test.
    payload = _run(
        tmp_path,
        monkeypatch,
        parameters={"engine_version": backtest.PREVIOUS_ENGINE_VERSION, "claim_signal_weight": 0.5},
    )

    assert payload["engine_version"] == backtest.PREVIOUS_ENGINE_VERSION
    assert payload["summary"]["position_tracking"] == "constant_weights"


def test_rejected_parameters_answer_with_422(tmp_path, monkeypatch) -> None:
    storage_key, checksum = _snapshot(tmp_path)
    monkeypatch.setenv("DOCUMENT_STORAGE_PATH", str(tmp_path))

    response = TestClient(app).post(
        "/v1/backtests",
        json={
            "backtest_id": "0a5f6f16-0a1e-4f0a-9d0e-2f2b4a0f6f11",
            "spec": SPEC,
            "snapshot_storage_key": storage_key,
            "snapshot_checksum": checksum,
            "document_cutoff_at": "2020-03-01T00:00:00Z",
            "parameters": {"cash_policy": "borrow"},
            "signals": [],
        },
    )

    assert response.status_code == 422
    assert "cash_policy" in response.text
