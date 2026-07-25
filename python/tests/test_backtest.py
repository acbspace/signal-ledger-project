import datetime as dt
import hashlib
import platform

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

V3 = {"engine_version": backtest.PREVIOUS_ENGINE_VERSION}


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


def _panel(scale: float = 1.0) -> pl.DataFrame:
    # Deterministic diverging trends so ranking is unambiguous: AAA > BBB > CCC.
    # ``scale`` compresses the spread without touching the ordering, which is how
    # the rank-space tests vary volatility while holding the ranking fixed.
    days = _business_days(dt.date(2020, 1, 1), 30)
    rows = []
    for index, day in enumerate(days):
        rows.append({"date": day, "symbol": "AAA", "close": 100.0 + scale * index})
        rows.append({"date": day, "symbol": "BBB", "close": 100.0 + scale * 0.5 * index})
        rows.append({"date": day, "symbol": "CCC", "close": 100.0 - scale * 0.3 * index})
    return pl.DataFrame(rows).with_columns(pl.col("date").cast(pl.Date))


def _volatile_panel() -> pl.DataFrame:
    """A panel with a drawdown deep enough to trigger a stop loss."""

    days = _business_days(dt.date(2020, 1, 1), 60)
    rows = []
    for index, day in enumerate(days):
        # AAA climbs then collapses; BBB grinds up; CCC drifts down.
        aaa = 100.0 + 2.0 * index if index < 30 else 160.0 - 3.0 * (index - 30)
        rows.append({"date": day, "symbol": "AAA", "close": aaa})
        rows.append({"date": day, "symbol": "BBB", "close": 100.0 + 0.4 * index})
        rows.append({"date": day, "symbol": "CCC", "close": 100.0 - 0.2 * index})
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


def _panel_with_extra_symbol() -> pl.DataFrame:
    """The three-symbol panel plus ZZZ, the strongest momentum name in the data."""

    days = _business_days(dt.date(2020, 1, 1), 30)
    rows = []
    for index, day in enumerate(days):
        rows.append({"date": day, "symbol": "AAA", "close": 100.0 + index})
        rows.append({"date": day, "symbol": "BBB", "close": 100.0 + 0.5 * index})
        rows.append({"date": day, "symbol": "CCC", "close": 100.0 - 0.3 * index})
        rows.append({"date": day, "symbol": "ZZZ", "close": 100.0 * (1.05**index)})
    return pl.DataFrame(rows).with_columns(pl.col("date").cast(pl.Date))


def test_engine_trades_only_the_committed_universe() -> None:
    # A snapshot is a shared resource and may carry series this strategy does not
    # trade. ZZZ would top the momentum ranking, but no reviewed spec authorized
    # it and no accepted claim cites it, so it must never be held or proposed.
    spec = {**SPEC, "universe": {**SPEC["universe"], "symbols": ["AAA", "BBB"]}}

    result = backtest.simulate(_panel_with_extra_symbol(), spec, {})

    for rebalance in result["holdings"]:
        assert "ZZZ" not in rebalance["weights"], rebalance
    assert set(result["holdings"][0]["weights"]) == {"AAA", "BBB"}
    assert [position["symbol"] for position in result["candidates"]["positions"]] == ["AAA", "BBB"]
    assert result["n_universe_symbols"] == 2


def test_snapshot_missing_a_universe_symbol_is_rejected() -> None:
    # Silently trading the subset that happens to be present would make the result
    # depend on data availability rather than on the committed spec.
    spec = {**SPEC, "universe": {**SPEC["universe"], "symbols": ["AAA", "BBB", "MISSING"]}}

    with pytest.raises(backtest.BacktestError, match="MISSING"):
        backtest.simulate(_panel(), spec, {})


def test_universe_must_be_present_and_non_empty() -> None:
    # This service is an independent HTTP boundary, so it validates rather than
    # trusting that Go already did.
    with pytest.raises(backtest.BacktestError):
        backtest.simulate(_panel(), {**SPEC, "universe": {"name": "U", "asset_class": "equity"}}, {})
    with pytest.raises(backtest.BacktestError):
        backtest.simulate(_panel(), {**SPEC, "universe": {**SPEC["universe"], "symbols": []}}, {})


def test_point_in_time_no_lookahead() -> None:
    panel = _panel()
    base = backtest.simulate(panel, SPEC, {})
    first_rebalance = base["holdings"][0]
    decided_at = dt.date.fromisoformat(first_rebalance["decided_at"])

    # Spike CCC on every day strictly AFTER the first rebalance was decided —
    # which now includes the day it fills on. A look-ahead bug would let that
    # future data change the holding chosen at the decision.
    mutated = panel.with_columns(
        pl.when((pl.col("date") > decided_at) & (pl.col("symbol") == "CCC"))
        .then(pl.col("close") * 100)
        .otherwise(pl.col("close"))
        .alias("close")
    )
    after = backtest.simulate(mutated, SPEC, {})

    assert after["holdings"][0] == first_rebalance


# Momentum is ranked cross-sectionally onto [-1, 1], so with three eligible names
# the ranks are exactly -1, 0 and +1. A weight of 1.5 therefore lets one
# full-confidence claim lift the bottom name past the middle one, but not past
# the top one — the same reordering v3 needed 0.5 of a trailing return for.
TILT = {"claim_signal_weight": 1.5}


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


def test_claim_signal_outside_the_universe_is_ignored() -> None:
    spec = {**SPEC, "universe": {**SPEC["universe"], "symbols": ["AAA", "BBB"]}}
    baseline = backtest.simulate(_panel_with_extra_symbol(), spec, TILT, [])

    # A claim may legitimately cite something the strategy does not trade; it stays
    # evidence without becoming a signal, and must not inflate the run's counters.
    tilted = backtest.simulate(
        _panel_with_extra_symbol(), spec, TILT, [_signal("ZZZ", "positive", 1.0, "2020-01-01")]
    )

    assert tilted["holdings"] == baseline["holdings"]
    assert tilted["n_claim_signals"] == 0
    assert tilted["n_claim_supported_rebalances"] == 0


def test_claim_confidence_filter_gates_weak_claims() -> None:
    panel = _panel()
    # A 0.4-confidence claim is worth 0.4 of the rank spread, so it takes a
    # weight above 2.5 to carry the bottom-ranked name past the middle one.
    tilt = {"claim_signal_weight": 3.0}
    baseline = backtest.simulate(panel, SPEC, tilt, [])
    weak = _signal("CCC", "positive", 0.4, "2020-01-01")

    admitted = backtest.simulate(panel, SPEC, tilt, [weak])
    gated = backtest.simulate(panel, SPEC, {**tilt, "claim_confidence": 0.5}, [weak])

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
    # CCC ranks -1 and AAA +1, so at weight 2.0 a full-confidence claim on CCC
    # (-1 + 2.0) outranks AAA carrying a mild negative one (+1 - 0.4).
    tilt = {"claim_signal_weight": 2.0}

    result = backtest.simulate(panel, SPEC, tilt, signals)
    candidates = result["candidates"]

    # as_of is the day the ranking was decided, which is what pairs it with the
    # run's document cutoff; the holding it produced fills a day later.
    assert candidates["as_of"] == result["holdings"][-1]["decided_at"]
    positions = candidates["positions"]
    assert [position["rank"] for position in positions] == [1, 2]
    # CCC's claim outweighs its negative momentum, so it ranks first.
    assert positions[0]["symbol"] == "CCC"
    assert positions[0]["claim_support"] == 1.0
    assert positions[0]["evidence"][0]["contribution"] == 1.0
    assert positions[0]["evidence"][0]["claim_id"] == signals[0]["claim_id"]
    # score = momentum_rank + weight * support, so the tilt stays auditable per
    # position: momentum is still the raw trailing return, momentum_rank is the
    # value that actually entered the ranking.
    assert positions[0]["momentum_rank"] == -1.0
    assert positions[0]["momentum"] < 0
    assert positions[0]["score"] == pytest.approx(
        positions[0]["momentum_rank"] + tilt["claim_signal_weight"] * positions[0]["claim_support"]
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
    assert result["engine_version"] == "momentum-claims-v4"
    assert len(result["checksum"]) == 64
    assert result["summary"]["n_rebalances"] >= 1
    # The summary records what research contributed, not just the price metrics.
    assert result["summary"]["n_claim_signals"] == 1
    assert result["summary"]["claim_signal_weight"] == 1.5
    # ...and the accounting the numbers were produced under, so a stored summary
    # explains itself without a reader looking up what the version decided.
    assert result["summary"]["position_tracking"] == "drift"
    assert result["summary"]["momentum_scale"] == "rank"
    assert result["summary"]["execution_lag_days"] == 1
    assert result["summary"]["cash_policy"] == "cash"
    assert result["summary"]["invested_fraction"] > 0
    # ...and the environment that produced the checksum, since the engine version
    # names the math alone. These are the versions requirements.txt pinned.
    assert result["summary"]["python_version"] == platform.python_version()
    assert result["summary"]["polars_version"] == pl.__version__
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


# ---------------------------------------------------------------------------
# Accounting
# ---------------------------------------------------------------------------

# Two symbols, both flat at 100 until the first fill, after which AAA steps to
# 150 and BBB stays put. That makes the whole simulation hand-computable:
#
#   decisions land on 2020-01-30 and 2020-02-03 (monthly, 21-day lookback),
#   so with a one-day lag the fills are 2020-01-31 and 2020-02-04.
#
#   fill 1: buy 0.5/0.5 from cash          -> traded 1.0,   turnover 1.0
#   drift : AAA x1.5                       -> 0.75 / 0.50,  equity 1.25
#   fill 2: back to 0.5/0.5 of 1.25        -> traded 0.25,  turnover 0.2
#
# Under v3 the second fill is free, because the book is assumed to still sit at
# the weights the first one set.
DRIFT_SPEC = {
    **SPEC,
    "universe": {"name": "U", "asset_class": "equity", "symbols": ["AAA", "BBB"]},
    "risk": {"max_position_weight": 1.0, "transaction_cost_bps": 0},
}


def _step_panel() -> pl.DataFrame:
    days = _business_days(dt.date(2020, 1, 1), 30)
    rows = []
    for index, day in enumerate(days):
        rows.append({"date": day, "symbol": "AAA", "close": 100.0 if index <= 22 else 150.0})
        rows.append({"date": day, "symbol": "BBB", "close": 100.0})
    return pl.DataFrame(rows).with_columns(pl.col("date").cast(pl.Date))


def test_drifting_positions_are_charged_at_the_next_rebalance() -> None:
    result = backtest.simulate(_step_panel(), DRIFT_SPEC, {}, [])

    assert result["n_rebalances"] == 2
    assert [holding["decided_at"] for holding in result["holdings"]] == ["2020-01-30", "2020-02-03"]
    assert [holding["date"] for holding in result["holdings"]] == ["2020-01-31", "2020-02-04"]
    # 1.0 to establish the book, then 0.2 to pull the drifted weights back to
    # target. v3 reported only the 1.0 and traded the rest of the run for free.
    assert result["metrics"]["total_turnover"] == pytest.approx(1.2)
    assert result["metrics"]["final_equity"] == pytest.approx(1.25)
    assert result["metrics"]["invested_fraction"] == pytest.approx(1.0)

    legacy = backtest.simulate(_step_panel(), DRIFT_SPEC, V3, [])
    assert legacy["metrics"]["total_turnover"] == pytest.approx(1.0)


def test_rebalancing_cost_is_charged_on_the_value_actually_traded() -> None:
    spec = {**DRIFT_SPEC, "risk": {"max_position_weight": 1.0, "transaction_cost_bps": 100}}

    result = backtest.simulate(_step_panel(), spec, {}, [])

    # fill 1: cost 1% of 1.0 traded            -> equity 0.99
    # drift : AAA x1.5                          -> equity 1.2375
    # fill 2: 0.2475 traded, cost 1% of that    -> equity 1.235025
    assert result["metrics"]["total_turnover"] == pytest.approx(1.2)
    assert result["metrics"]["final_equity"] == pytest.approx(1.235025)


def test_execution_lag_fills_after_the_bar_that_decided_the_trade() -> None:
    lagged = backtest.simulate(_step_panel(), DRIFT_SPEC, {}, [])
    immediate = backtest.simulate(_step_panel(), DRIFT_SPEC, {"execution_lag_days": 0}, [])

    assert lagged["execution_lag_days"] == 1
    assert immediate["execution_lag_days"] == 0
    for holding in lagged["holdings"]:
        assert holding["date"] > holding["decided_at"]
    for holding in immediate["holdings"]:
        assert holding["date"] == holding["decided_at"]

    # On a panel that moves every day, paying the next close instead of the one
    # the decision was read off cannot produce the same curve.
    assert backtest.equity_curve_csv(
        backtest.simulate(_panel(), SPEC, {}, [])
    ) != backtest.equity_curve_csv(
        backtest.simulate(_panel(), SPEC, {"execution_lag_days": 0}, [])
    )


def test_execution_lag_beyond_the_panel_never_fills() -> None:
    # Weekly decisions land on 2020-01-30, 2020-02-03 and 2020-02-10; at a
    # five-day lag the last has no bar left to fill on. It is not a rebalance
    # that happened, so it must not be counted or proposed as one.
    spec = {**DRIFT_SPEC, "rebalance": {"schedule": "weekly"}}

    result = backtest.simulate(_step_panel(), spec, {"execution_lag_days": 5}, [])

    assert [holding["decided_at"] for holding in result["holdings"]] == ["2020-01-30", "2020-02-03"]
    assert result["n_rebalances"] == 2
    assert result["candidates"]["as_of"] == "2020-02-03"


STOP_SPEC = {
    **SPEC,
    "rebalance": {"schedule": "weekly"},
    "risk": {"max_position_weight": 0.6, "transaction_cost_bps": 10, "stop_loss_pct": 0.1},
}
NO_STOP_SPEC = {**STOP_SPEC, "risk": {"max_position_weight": 0.6, "transaction_cost_bps": 10}}


def test_stop_loss_exit_is_charged() -> None:
    stopped = backtest.simulate(_volatile_panel(), STOP_SPEC, {}, [])
    unstopped = backtest.simulate(_volatile_panel(), NO_STOP_SPEC, {}, [])

    assert stopped["n_stop_loss_exits"] > 0
    # A forced sale is a trade: it has to show up as turnover. v3 zeroed the
    # weight for free, which made turnover *fall* when a stop loss was enabled.
    assert stopped["metrics"]["total_turnover"] > unstopped["metrics"]["total_turnover"]


def test_the_preserved_engine_still_shows_the_defect_it_was_frozen_with() -> None:
    # Not an endorsement — a guard. The previous accounting is kept to reproduce
    # runs made under it, so its defects have to survive intact or the runs it
    # reproduces are not the runs that happened.
    stopped = backtest.simulate(_volatile_panel(), STOP_SPEC, V3, [])
    unstopped = backtest.simulate(_volatile_panel(), NO_STOP_SPEC, V3, [])

    assert stopped["metrics"]["total_turnover"] < unstopped["metrics"]["total_turnover"]


CAPPED_SPEC = {**SPEC, "risk": {"max_position_weight": 0.34, "transaction_cost_bps": 10}}


def test_cash_policy_cash_reports_the_uninvested_remainder() -> None:
    result = backtest.simulate(_panel(), CAPPED_SPEC, {}, [])

    # top_n is 2 and the cap is 0.34, so the book can only ever be 68% invested.
    # Left unreported this reads as a strategy that barely moves.
    weights = result["holdings"][0]["weights"]
    assert sorted(weights.values()) == [0.34, 0.34]
    assert result["cash_policy"] == "cash"
    assert result["metrics"]["invested_fraction"] < 0.75


def test_cash_policy_extend_fills_the_book_without_breaching_the_cap() -> None:
    result = backtest.simulate(_panel(), CAPPED_SPEC, {"cash_policy": "extend"}, [])

    weights = result["holdings"][0]["weights"]
    # Being fully invested under a 0.34 cap takes three names, so the cap decides
    # the count instead of quietly leaving a third of the book in cash.
    assert len(weights) == 3
    assert all(weight <= 0.34 for weight in weights.values())
    assert sum(weights.values()) == pytest.approx(1.0)
    assert result["metrics"]["invested_fraction"] == pytest.approx(1.0, abs=0.05)


def test_extend_cannot_invent_names_the_universe_does_not_have() -> None:
    # A 0.2 cap wants five names and the universe has three, so the book still
    # runs partly in cash — the universe's doing, and the summary says so.
    spec = {**SPEC, "risk": {"max_position_weight": 0.2, "transaction_cost_bps": 10}}

    result = backtest.simulate(_panel(), spec, {"cash_policy": "extend"}, [])

    weights = result["holdings"][0]["weights"]
    assert len(weights) == 3
    assert sum(weights.values()) == pytest.approx(0.6)
    assert result["metrics"]["invested_fraction"] < 0.8


# ---------------------------------------------------------------------------
# The research tilt
# ---------------------------------------------------------------------------


def test_rank_space_makes_the_tilt_independent_of_volatility() -> None:
    # Two panels with identical rankings and a tenfold difference in spread. In
    # return space the same claim at the same weight decides the book in one and
    # is ignored in the other; the tilt's real influence was never the weight,
    # it was the weight divided by whatever the assets happened to be doing.
    signals = [_signal("CCC", "positive", 1.0, "2020-01-01", horizon=400)]

    wide = backtest.simulate(_panel(), SPEC, {**V3, "claim_signal_weight": 0.15}, signals)
    narrow = backtest.simulate(_panel(0.1), SPEC, {**V3, "claim_signal_weight": 0.15}, signals)
    assert "CCC" not in wide["holdings"][0]["weights"]
    assert "CCC" in narrow["holdings"][0]["weights"]

    # Rank space spreads momentum over [-1, 1] whatever the volatility, so the
    # same weight buys the same influence in both.
    wide_ranked = backtest.simulate(_panel(), SPEC, TILT, signals)
    narrow_ranked = backtest.simulate(_panel(0.1), SPEC, TILT, signals)
    assert set(wide_ranked["holdings"][0]["weights"]) == {"AAA", "CCC"}
    assert set(narrow_ranked["holdings"][0]["weights"]) == {"AAA", "CCC"}


def test_tied_momentum_shares_a_rank() -> None:
    # BBB and CCC are the same series under different names. Breaking the tie by
    # rank would let the tilt turn on alphabetical order.
    days = _business_days(dt.date(2020, 1, 1), 30)
    rows = []
    for index, day in enumerate(days):
        rows.append({"date": day, "symbol": "AAA", "close": 100.0 + index})
        rows.append({"date": day, "symbol": "BBB", "close": 100.0 - 0.3 * index})
        rows.append({"date": day, "symbol": "CCC", "close": 100.0 - 0.3 * index})
    panel = pl.DataFrame(rows).with_columns(pl.col("date").cast(pl.Date))

    result = backtest.simulate(panel, {**SPEC, "selection": {**SPEC["selection"], "filters": [
        {"field": "lookback_months", "operator": "eq", "value": 1},
        {"field": "top_n", "operator": "eq", "value": 3},
    ]}}, {}, [])

    ranks = {
        position["symbol"]: position["momentum_rank"]
        for position in result["candidates"]["positions"]
    }
    assert ranks["BBB"] == ranks["CCC"]
    assert ranks["AAA"] > ranks["BBB"]


# ---------------------------------------------------------------------------
# The preserved accounting
# ---------------------------------------------------------------------------

# Curves captured from momentum-claims-v3 before v4 changed the accounting. They
# are what makes "the previous path is still reachable" a checkable claim rather
# than an intention: if any of these move, a run made under v3 can no longer be
# reproduced and the diff that justifies v4 is not a diff of the same thing.
V3_CURVES = {
    "baseline": "214e7509a8517ebae92b1684cb5104116623d0bb7a5b7c665e16a35ebc9e96d3",
    "tilted": "19799904908cfe33f93cf7a5ece13324ee41c4bcd152fd911fed0445f051e75b",
    "stop_loss": "237c9f610c2186d70d6b02c5f0eae33763164166a42cafe30dffd856041d3630",
    "capped": "75da926bf7e63b8da6a227f2e66b9cc1f789d15e21a69374670fa270c771c7e7",
}


def _v3_scenarios() -> dict[str, dict]:
    return {
        "baseline": backtest.simulate(_panel(), SPEC, V3, []),
        "tilted": backtest.simulate(
            _panel(),
            SPEC,
            {**V3, "claim_signal_weight": 0.5},
            [_signal("CCC", "positive", 1.0, "2020-01-01", horizon=400)],
        ),
        "stop_loss": backtest.simulate(_volatile_panel(), STOP_SPEC, V3, []),
        "capped": backtest.simulate(
            _panel(), {**SPEC, "risk": {"max_position_weight": 0.2, "transaction_cost_bps": 10}}, V3, []
        ),
    }


@pytest.mark.parametrize("scenario", sorted(V3_CURVES))
def test_previous_engine_reproduces_its_recorded_curves(scenario: str) -> None:
    result = _v3_scenarios()[scenario]
    curve = backtest.equity_curve_csv(result)

    assert hashlib.sha256(curve.encode("utf-8")).hexdigest() == V3_CURVES[scenario]
    assert result["engine_version"] == backtest.PREVIOUS_ENGINE_VERSION
    assert result["position_tracking"] == "constant_weights"
    assert result["momentum_scale"] == "return"


def test_v4_changes_the_curve_it_was_meant_to_change() -> None:
    # The point of the version bump. If these matched, the accounting fixes did
    # nothing and the new version would be noise.
    for scenario, result in _v3_scenarios().items():
        current = {
            "baseline": lambda: backtest.simulate(_panel(), SPEC, {}, []),
            "tilted": lambda: backtest.simulate(
                _panel(),
                SPEC,
                {"claim_signal_weight": 0.5},
                [_signal("CCC", "positive", 1.0, "2020-01-01", horizon=400)],
            ),
            "stop_loss": lambda: backtest.simulate(_volatile_panel(), STOP_SPEC, {}, []),
            "capped": lambda: backtest.simulate(
                _panel(), {**SPEC, "risk": {"max_position_weight": 0.2, "transaction_cost_bps": 10}}, {}, []
            ),
        }[scenario]()
        assert backtest.equity_curve_csv(current) != backtest.equity_curve_csv(result), scenario


def test_previous_engine_refuses_configuration_it_never_had() -> None:
    # Applying a v4 knob to v3's accounting would produce a curve that never
    # existed, which defeats the only reason v3 is still here.
    with pytest.raises(backtest.BacktestError, match="execution_lag_days"):
        backtest.simulate(_panel(), SPEC, {**V3, "execution_lag_days": 1}, [])
    with pytest.raises(backtest.BacktestError, match="cash_policy"):
        backtest.simulate(_panel(), SPEC, {**V3, "cash_policy": "extend"}, [])


def test_unknown_engine_version_is_rejected() -> None:
    with pytest.raises(backtest.BacktestError, match="unsupported engine_version"):
        backtest.simulate(_panel(), SPEC, {"engine_version": "momentum-claims-v9"}, [])


def test_invalid_accounting_parameters_are_rejected() -> None:
    with pytest.raises(backtest.BacktestError, match="execution_lag_days"):
        backtest.simulate(_panel(), SPEC, {"execution_lag_days": -1}, [])
    with pytest.raises(backtest.BacktestError, match="execution_lag_days"):
        backtest.simulate(_panel(), SPEC, {"execution_lag_days": 99}, [])
    with pytest.raises(backtest.BacktestError, match="cash_policy"):
        backtest.simulate(_panel(), SPEC, {"cash_policy": "borrow"}, [])


# ---------------------------------------------------------------------------
# Invariants
# ---------------------------------------------------------------------------

INVARIANT_CASES = [
    ("default", SPEC, {}),
    ("legacy", SPEC, V3),
    ("no lag", SPEC, {"execution_lag_days": 0}),
    ("long lag", SPEC, {"execution_lag_days": 3}),
    ("extend", CAPPED_SPEC, {"cash_policy": "extend"}),
    ("capped", CAPPED_SPEC, {}),
    ("stop loss", STOP_SPEC, {}),
    ("one name", SPEC, {"top_n": 1}),
    ("whole universe", SPEC, {"top_n": 3}),
    ("gated", SPEC, {"require_claim_support": True}),
]


@pytest.mark.parametrize("name,spec,parameters", INVARIANT_CASES, ids=[case[0] for case in INVARIANT_CASES])
def test_accounting_invariants_hold(name: str, spec: dict, parameters: dict) -> None:
    panel = _volatile_panel() if spec is STOP_SPEC else _panel()
    signals = [
        _signal("CCC", "positive", 0.7, "2020-01-01", horizon=400),
        _signal("AAA", "negative", 0.3, "2020-01-15", horizon=400),
    ]

    result = backtest.simulate(panel, spec, parameters, signals)

    max_weight = spec["risk"]["max_position_weight"]
    for holding in result["holdings"]:
        weights = holding["weights"]
        # A long-only book cannot be more than fully invested, and no position
        # may breach the committed cap.
        assert sum(weights.values()) <= 1.0 + 1e-9, holding
        assert all(0 < weight <= max_weight + 1e-9 for weight in weights.values()), holding

    assert result["metrics"]["total_turnover"] >= 0
    assert 0.0 <= result["metrics"]["invested_fraction"] <= 1.0 + 1e-9
    assert all(equity > 0 for _, equity in result["equity_curve"])
    assert len(result["equity_curve"]) == result["n_trading_days"]


@pytest.mark.parametrize("name,spec,parameters", INVARIANT_CASES, ids=[case[0] for case in INVARIANT_CASES])
def test_results_are_stable_under_signal_reordering(name: str, spec: dict, parameters: dict) -> None:
    panel = _volatile_panel() if spec is STOP_SPEC else _panel()
    signals = [
        _signal("CCC", "positive", 0.7, "2020-01-01", horizon=400),
        _signal("AAA", "negative", 0.3, "2020-01-15", horizon=400),
        _signal("BBB", "positive", 0.5, "2020-01-08", horizon=400),
    ]

    forward = backtest.simulate(panel, spec, parameters, signals)
    backward = backtest.simulate(panel, spec, parameters, list(reversed(signals)))

    assert backtest.equity_curve_csv(forward) == backtest.equity_curve_csv(backward)


# ---------------------------------------------------------------------------
# Benchmarks
# ---------------------------------------------------------------------------

# One symbol, no cap, no cost, no lag: the strategy buys everything at the first
# fill and holds, which is exactly what the equal-weight benchmark does. Their
# return series have to match term for term.
SOLO_SPEC = {
    **SPEC,
    "universe": {"name": "U", "asset_class": "equity", "symbols": ["AAA"]},
    "risk": {"max_position_weight": 1.0, "transaction_cost_bps": 0},
}


def test_strategy_that_is_the_benchmark_has_no_alpha() -> None:
    result = backtest.simulate(_panel(), SOLO_SPEC, {"execution_lag_days": 0}, [])

    equal_weight = result["benchmarks"]["equal_weight_universe"]
    assert equal_weight["total_return"] == result["metrics"]["total_return"]
    assert equal_weight["beta"] == pytest.approx(1.0)
    assert equal_weight["alpha"] == pytest.approx(0.0, abs=1e-9)
    assert equal_weight["tracking_error"] == pytest.approx(0.0, abs=1e-9)
    assert equal_weight["information_ratio"] == pytest.approx(0.0, abs=1e-9)


def test_selection_is_measured_against_holding_the_whole_universe() -> None:
    # CCC falls all run, so dropping it should beat holding all three names.
    result = backtest.simulate(_panel(), SPEC, {}, [])

    equal_weight = result["benchmarks"]["equal_weight_universe"]
    assert result["metrics"]["total_return"] > equal_weight["total_return"]
    assert equal_weight["information_ratio"] > 0


def test_benchmark_symbol_is_read_but_never_traded() -> None:
    # ZZZ tops the momentum ranking and is not in the universe. It has to show up
    # as a benchmark and stay out of the book (ADR 0007).
    spec = {**SPEC, "universe": {**SPEC["universe"], "symbols": ["AAA", "BBB"]}}

    result = backtest.simulate(_panel_with_extra_symbol(), spec, {"benchmark_symbol": "ZZZ"}, [])

    assert result["benchmark_symbol_priced"] is True
    assert "ZZZ" in result["benchmarks"]
    assert result["benchmarks"]["ZZZ"]["total_return"] > 0
    for holding in result["holdings"]:
        assert "ZZZ" not in holding["weights"], holding
    assert all(position["symbol"] != "ZZZ" for position in result["candidates"]["positions"])


def test_an_unpriced_benchmark_says_so_rather_than_vanishing() -> None:
    result = backtest.simulate(_panel(), SPEC, {"benchmark_symbol": "SPY"}, [])

    assert result["benchmark_symbol"] == "SPY"
    assert result["benchmark_symbol_priced"] is False
    assert "SPY" not in result["benchmarks"]
    # The universe baseline needs no extra data, so it is always there.
    assert "equal_weight_universe" in result["benchmarks"]


def test_risk_free_rate_is_charged_to_sharpe_and_alpha() -> None:
    free = backtest.simulate(_panel(), SPEC, {}, [])
    charged = backtest.simulate(_panel(), SPEC, {"risk_free_rate": 0.05}, [])

    assert free["metrics"]["sharpe"] > charged["metrics"]["sharpe"]
    assert charged["risk_free_rate"] == 0.05
    # Beta is a covariance ratio, so the rate cannot move it.
    assert free["benchmarks"]["equal_weight_universe"]["beta"] == pytest.approx(
        charged["benchmarks"]["equal_weight_universe"]["beta"]
    )


def test_risk_free_rate_is_validated_as_a_decimal() -> None:
    with pytest.raises(backtest.BacktestError, match="risk_free_rate"):
        backtest.simulate(_panel(), SPEC, {"risk_free_rate": 5.0}, [])
    with pytest.raises(backtest.BacktestError, match="risk_free_rate"):
        backtest.simulate(_panel(), SPEC, {"risk_free_rate": -0.01}, [])


def test_benchmarks_do_not_change_the_curve_they_measure() -> None:
    # Reporting only. If a benchmark could move the equity curve it would change
    # result_checksum, and ENGINE_VERSION would have to move with it.
    plain = backtest.simulate(_panel_with_extra_symbol(), SPEC, {}, [])
    measured = backtest.simulate(_panel_with_extra_symbol(), SPEC, {"benchmark_symbol": "ZZZ"}, [])

    assert backtest.equity_curve_csv(plain) == backtest.equity_curve_csv(measured)
