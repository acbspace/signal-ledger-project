import datetime as dt
import hashlib

import polars as pl
import pytest

from quant_service import backtest

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


def _signal(symbol: str, direction: str, confidence: float, effective: str, horizon: int | None = None) -> dict:
    return {
        "claim_id": f"{symbol}-{direction}-{effective}",
        "symbol": symbol,
        "direction": direction,
        "confidence": confidence,
        "effective_at": f"{effective}T00:00:00Z",
        "horizon_days": horizon,
    }


def _business_days(start: dt.date, count: int) -> list[dt.date]:
    days: list[dt.date] = []
    day = start
    while len(days) < count:
        if day.weekday() < 5:
            days.append(day)
        day += dt.timedelta(days=1)
    return days


def _panel() -> pl.DataFrame:
    # Deterministic diverging trends so ranking is unambiguous: AAA > BBB > CCC.
    days = _business_days(dt.date(2020, 1, 1), 30)
    rows = []
    for index, day in enumerate(days):
        rows.append({"date": day, "symbol": "AAA", "close": 100.0 + index})
        rows.append({"date": day, "symbol": "BBB", "close": 100.0 + 0.5 * index})
        rows.append({"date": day, "symbol": "CCC", "close": 100.0 - 0.3 * index})
    return pl.DataFrame(rows).with_columns(pl.col("date").cast(pl.Date))


def _canonical_csv(panel: pl.DataFrame) -> bytes:
    ordered = panel.sort(["symbol", "date"])
    lines = ["symbol,date,open,high,low,close,adj_close,volume"]
    for row in ordered.iter_rows(named=True):
        price = row["close"]
        lines.append(f"{row['symbol']},{row['date'].isoformat()},{price},{price},{price},{price},{price},100")
    return ("\n".join(lines) + "\n").encode("utf-8")


def test_backtest_is_deterministic() -> None:
    panel = _panel()
    first = backtest.simulate(panel, SPEC, {})
    second = backtest.simulate(panel, SPEC, {})

    assert first["n_rebalances"] >= 1
    assert first["metrics"] == second["metrics"]
    # Byte-identical equity curve is the strongest determinism assertion.
    assert backtest.equity_curve_csv(first) == backtest.equity_curve_csv(second)
    # Rising AAA and BBB should be selected over falling CCC.
    assert set(first["holdings"][0]["weights"]) == {"AAA", "BBB"}


def test_point_in_time_no_lookahead() -> None:
    panel = _panel()
    base = backtest.simulate(panel, SPEC, {})
    first_rebalance = base["holdings"][0]
    rebalance_date = dt.date.fromisoformat(first_rebalance["date"])

    # Spike CCC on every day strictly AFTER the first rebalance. A look-ahead bug
    # would let that future data change the holding chosen at the rebalance.
    mutated = panel.with_columns(
        pl.when((pl.col("date") > rebalance_date) & (pl.col("symbol") == "CCC"))
        .then(pl.col("close") * 100)
        .otherwise(pl.col("close"))
        .alias("close")
    )
    after = backtest.simulate(mutated, SPEC, {})

    assert after["holdings"][0] == first_rebalance


# Momentum at the first rebalance is roughly AAA +0.21, BBB +0.10, CCC -0.06, so
# a 0.5 weight lets one full-confidence claim reorder the ranking outright.
TILT = {"claim_signal_weight": 0.5}


def test_claim_signal_tilts_selection() -> None:
    panel = _panel()
    baseline = backtest.simulate(panel, SPEC, TILT, [])
    assert set(baseline["holdings"][0]["weights"]) == {"AAA", "BBB"}

    tilted = backtest.simulate(panel, SPEC, TILT, [_signal("CCC", "positive", 1.0, "2020-01-01")])

    # Research support outweighs CCC's negative momentum and displaces BBB.
    assert set(tilted["holdings"][0]["weights"]) == {"AAA", "CCC"}
    assert tilted["holdings"][0]["claim_support"]["CCC"] == 1.0
    assert tilted["n_claim_signals"] == 1
    assert tilted["n_claim_supported_rebalances"] == tilted["n_rebalances"]


def test_claim_signal_is_ignored_before_it_is_effective() -> None:
    panel = _panel()
    baseline = backtest.simulate(panel, SPEC, TILT, [])
    assert len(baseline["holdings"]) >= 2

    # Effective between the first and second rebalance: a look-ahead bug would
    # let it reach back and change the first one.
    later = backtest.simulate(panel, SPEC, TILT, [_signal("CCC", "positive", 1.0, "2020-02-01")])

    assert later["holdings"][0] == baseline["holdings"][0]
    assert set(later["holdings"][1]["weights"]) == {"AAA", "CCC"}


def test_claim_signal_expires_after_its_horizon() -> None:
    panel = _panel()
    baseline = backtest.simulate(panel, SPEC, TILT, [])

    # Live 2020-01-01 through 2020-01-20; every rebalance happens after that.
    expired = backtest.simulate(
        panel, SPEC, TILT, [_signal("CCC", "positive", 1.0, "2020-01-01", horizon=20)]
    )

    assert expired["holdings"] == baseline["holdings"]
    assert expired["n_claim_supported_rebalances"] == 0


def test_require_claim_support_holds_only_researched_symbols() -> None:
    panel = _panel()
    gated = backtest.simulate(
        panel,
        SPEC,
        {"claim_signal_weight": 0.5, "require_claim_support": True},
        [_signal("CCC", "positive", 1.0, "2020-01-01"), _signal("AAA", "negative", 0.4, "2020-01-01")],
    )

    # BBB has no claim and AAA's nets negative, so only CCC stays eligible.
    assert set(gated["holdings"][0]["weights"]) == {"CCC"}


def test_neutral_and_offsetting_claims_do_not_tilt() -> None:
    panel = _panel()
    baseline = backtest.simulate(panel, SPEC, TILT, [])

    neutral = backtest.simulate(panel, SPEC, TILT, [_signal("CCC", "neutral", 1.0, "2020-01-01")])
    offsetting = backtest.simulate(
        panel,
        SPEC,
        TILT,
        [_signal("CCC", "positive", 0.8, "2020-01-01"), _signal("CCC", "negative", 0.8, "2020-01-01")],
    )

    assert neutral["holdings"] == baseline["holdings"]
    assert offsetting["holdings"] == baseline["holdings"]


def test_claim_confidence_filter_gates_weak_claims() -> None:
    panel = _panel()
    baseline = backtest.simulate(panel, SPEC, TILT, [])
    weak = _signal("CCC", "positive", 0.4, "2020-01-01")

    admitted = backtest.simulate(panel, SPEC, TILT, [weak])
    gated = backtest.simulate(panel, SPEC, {**TILT, "claim_confidence": 0.5}, [weak])

    assert admitted["holdings"][0]["claim_support"]["CCC"] == 0.4
    assert gated["holdings"] == baseline["holdings"]
    assert gated["n_claim_signals"] == 0


def test_claim_signal_order_does_not_change_results() -> None:
    panel = _panel()
    signals = [
        _signal("CCC", "positive", 0.7, "2020-01-01"),
        _signal("AAA", "negative", 0.3, "2020-01-02"),
        _signal("BBB", "positive", 0.5, "2020-01-01", horizon=400),
    ]

    forward = backtest.simulate(panel, SPEC, TILT, signals)
    reversed_order = backtest.simulate(panel, SPEC, TILT, list(reversed(signals)))

    # The engine sorts signals itself, so accumulation order — and therefore the
    # curve bytes — cannot depend on how the worker happened to list them.
    assert backtest.equity_curve_csv(forward) == backtest.equity_curve_csv(reversed_order)


def test_candidates_rank_the_last_rebalance_with_evidence() -> None:
    panel = _panel()
    signals = [
        _signal("CCC", "positive", 1.0, "2020-01-01", horizon=400),
        _signal("AAA", "negative", 0.2, "2020-01-01", horizon=400),
    ]

    result = backtest.simulate(panel, SPEC, TILT, signals)
    candidates = result["candidates"]

    assert candidates["as_of"] == result["holdings"][-1]["date"]
    positions = candidates["positions"]
    assert [position["rank"] for position in positions] == [1, 2]
    # CCC's claim outweighs its negative momentum, so it ranks first.
    assert positions[0]["symbol"] == "CCC"
    assert positions[0]["claim_support"] == 1.0
    assert positions[0]["evidence"][0]["contribution"] == 1.0
    assert positions[0]["evidence"][0]["claim_id"] == signals[0]["claim_id"]
    # score = momentum + weight * support, so the tilt is auditable per position.
    assert positions[0]["score"] == pytest.approx(
        positions[0]["momentum"] + TILT["claim_signal_weight"] * positions[0]["claim_support"]
    )
    # AAA is held on momentum despite a mildly negative claim, and says so.
    assert positions[1]["symbol"] == "AAA"
    assert positions[1]["claim_support"] == -0.2


def test_candidates_are_empty_when_nothing_is_selected() -> None:
    panel = _panel()
    # No symbol can clear a 100% trailing-return gate, so the run holds cash.
    spec = {**SPEC, "selection": {**SPEC["selection"], "filters": [
        {"field": "lookback_months", "operator": "eq", "value": 1},
        {"field": "momentum", "operator": "gt", "value": 1.0},
    ]}}

    result = backtest.simulate(panel, spec, {}, [])

    assert result["candidates"]["positions"] == []


def test_run_backtest_verifies_checksum(tmp_path) -> None:
    panel = _panel()
    csv = _canonical_csv(panel)
    checksum = hashlib.sha256(csv).hexdigest()
    storage_key = "market-data/snapshot.csv"
    path = tmp_path / storage_key
    path.parent.mkdir(parents=True)
    path.write_bytes(csv)

    result = backtest.run_backtest(
        snapshot_root=tmp_path,
        spec=SPEC,
        snapshot_storage_key=storage_key,
        snapshot_checksum=checksum,
        document_cutoff_at="2020-01-01T00:00:00Z",
        parameters=TILT,
        signals=[_signal("CCC", "positive", 1.0, "2020-01-01")],
    )
    assert result["engine_version"] == "momentum-claims-v2"
    assert len(result["checksum"]) == 64
    assert result["summary"]["n_rebalances"] >= 1
    # The summary records what research contributed, not just the price metrics.
    assert result["summary"]["n_claim_signals"] == 1
    assert result["summary"]["claim_signal_weight"] == 0.5
    # The returned checksum is the checksum of the artifact bytes the worker stores.
    curve = result["equity_curve_csv"]
    assert curve.startswith("date,equity\n")
    assert hashlib.sha256(curve.encode("utf-8")).hexdigest() == result["checksum"]

    # Any tampering with the frozen snapshot must be rejected.
    path.write_bytes(csv + b"AAA,2020-01-02,1,1,1,1,1,1\n")
    with pytest.raises(backtest.BacktestError):
        backtest.run_backtest(
            snapshot_root=tmp_path,
            spec=SPEC,
            snapshot_storage_key=storage_key,
            snapshot_checksum=checksum,
            document_cutoff_at="2020-01-01T00:00:00Z",
            parameters={},
        )
