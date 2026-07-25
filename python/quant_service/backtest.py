"""Deterministic backtest over price momentum tilted by point-in-time research.

Selection ranks the universe by trailing-return momentum and then tilts that rank
by the research claims that were already effective on the rebalance day. Two
independent no-look-ahead rules hold: a price decision uses only bars at or
before that day, and a claim only counts from its ``effective_at`` until its
horizon expires. The Go worker additionally drops any claim effective after the
run's document cutoff, so the engine never even receives future research.

The tradable set is ``spec["universe"]["symbols"]`` and nothing else. A snapshot
may legitimately carry more series than one strategy trades — snapshots are
shared resources — but a symbol the committed spec never authorized has no
reviewed research behind it, so it must not be rankable and must not become a
candidate. The snapshot must cover the universe; extra columns are ignored.

Given the same frozen snapshot, spec, and signals it produces byte-identical
results, so `run_backtest` verifies the snapshot checksum before simulating and
hashes the equity curve as the result checksum.

The snapshot is the canonical CSV frozen by the Go worker (columns:
symbol,date,open,high,low,close,adj_close,volume). This process stays stateless:
it reads the snapshot read-only and returns results for Go to persist.

`ENGINE_VERSION` must change whenever the simulation math changes. It names the
math only, so the run summary also records the interpreter and polars versions
that executed it — a result checksum identifies an environment, not just a
version string.

Accounting
----------

A simulated book is only worth reading if what it charges matches what it does.
``momentum-claims-v4`` fixes four places where v3's accounting and its behaviour
disagreed, and normalizes the research tilt:

* **Positions drift between rebalances.** v3 held target weights constant while
  prices moved, which is a rebalance to target every single day, yet it charged
  turnover only on rebalance dates — so the simulation traded daily for free.
  v4 marks each holding to market and only trades when an order fills.
* **A stop loss pays for its exit.** v3 zeroed the weight outside the rebalance
  branch, so the forced sale cost nothing and never entered ``total_turnover``;
  because the zeroed weight persisted, enabling a stop loss *reduced* reported
  turnover. v4 sends the exit as an order and charges it like any other.
* **The weight remainder is explicit.** ``min(1/n, max_position_weight)`` caps a
  position without redistributing, so any ``max_position_weight < 1/top_n``
  quietly ran a mostly-uninvested book that looked like a flat strategy. v4 has
  a ``cash_policy`` and reports ``invested_fraction`` in every summary.
* **Orders fill after the decision.** v3 read momentum and filled at the same
  bar, so a strategy needed that day's close to place a trade it filled at that
  close. v4 defaults ``execution_lag_days`` to 1.
* **The tilt is normalized.** ``momentum + weight * support`` adds a confidence
  to a return, so the tilt's real influence drifted with each asset's
  volatility. v4 ranks momentum cross-sectionally onto [-1, 1] first, which
  gives ``claim_signal_weight`` one meaning everywhere.

Portfolio-level sums use `math.fsum`, so a result cannot depend on the order a
dict or set happened to iterate in.

`PREVIOUS_ENGINE_VERSION` stays selectable through the ``engine_version`` run
parameter so a known strategy can be re-run under both and the curves diffed.
It reproduces v3 byte-for-byte and is frozen — see `_simulate_constant_weights`.
"""

from __future__ import annotations

import datetime as dt
import hashlib
import io
import math
import platform
import statistics
from dataclasses import dataclass
from pathlib import Path
from typing import Any, NamedTuple

import polars as pl

ENGINE_VERSION = "momentum-claims-v4"
# Reachable through the `engine_version` run parameter, so a run under the
# current accounting can be diffed against the accounting that preceded it.
PREVIOUS_ENGINE_VERSION = "momentum-claims-v3"

_TRADING_DAYS_PER_YEAR = 252
_TRADING_DAYS_PER_MONTH = 21

# How long a claim stays live when it did not state its own horizon.
_DEFAULT_CLAIM_HORIZON_DAYS = 90
# A lag exists to stop a decision filling on the bar that produced it. Beyond a
# week it is no longer modelling execution, so it is almost certainly a mistake.
_MAX_EXECUTION_LAG_DAYS = 5


class BacktestError(ValueError):
    """The backtest cannot be run for the given inputs."""


@dataclass(frozen=True)
class AccountingRules:
    """The simulation math one ``ENGINE_VERSION`` names.

    Every field here changes reported numbers, which is why they are selected by
    version rather than set individually: a run states which accounting produced
    it, instead of a combination of knobs nobody has evaluated together.
    """

    position_tracking: str
    charge_stop_loss_exit: bool
    momentum_scale: str
    execution_lag_days: int
    signal_weight: float


_ACCOUNTING: dict[str, AccountingRules] = {
    ENGINE_VERSION: AccountingRules(
        position_tracking="drift",
        charge_stop_loss_exit=True,
        momentum_scale="rank",
        execution_lag_days=1,
        # In rank space a full-confidence claim is worth this fraction of the
        # whole momentum spread. v3's 0.1 meant "ten points of trailing return";
        # carrying that number over unchanged would have quietly cut research
        # influence to a twentieth of the ranking, which is the opposite of what
        # normalizing the tilt is for.
        signal_weight=0.25,
    ),
    PREVIOUS_ENGINE_VERSION: AccountingRules(
        position_tracking="constant_weights",
        charge_stop_loss_exit=False,
        momentum_scale="return",
        execution_lag_days=0,
        signal_weight=0.1,
    ),
}

_CASH_POLICIES = ("cash", "extend")


class ClaimSignal(NamedTuple):
    """One accepted claim, resolved to a symbol and a live date window.

    ``score`` is signed confidence: positive claims push a symbol up the ranking,
    negative ones push it down. ``expires`` is exclusive.
    """

    claim_id: str
    symbol: str
    score: float
    effective: dt.date
    expires: dt.date


class Position(NamedTuple):
    """One selected symbol at a rebalance, with the inputs that selected it.

    ``momentum`` is always the raw trailing return, so it stays readable next to
    a price chart. ``momentum_rank`` is the value that actually entered the
    ranking, so ``score == momentum_rank + claim_signal_weight * claim_support``
    holds under either momentum scale and a stored position can be re-derived.
    """

    symbol: str
    weight: float
    score: float
    momentum: float
    momentum_rank: float
    claim_support: float


class Decision(NamedTuple):
    """A rebalance decided on ``as_of``'s close, with the research behind it."""

    as_of: dt.date
    positions: list[Position]
    active_claims: dict[str, list[ClaimSignal]]
    had_claim_support: bool


class Order(NamedTuple):
    """An instruction decided on one close and filled at ``execute_at``'s close.

    ``kind`` is ``rebalance`` (``weights`` replaces the whole book) or
    ``stop_exit`` (flatten the single symbol in ``weights``).
    """

    execute_at: int
    kind: str
    weights: dict[str, float]
    decision: Decision | None


@dataclass(frozen=True)
class EngineParams:
    lookback_months: int
    top_n: int | None
    gates: list[tuple[str, float]]
    max_weight: float
    cost_bps: float
    stop_loss: float | None
    signal_weight: float
    require_claim_support: bool
    claim_horizon_days: int
    min_claim_confidence: float
    engine_version: str
    rules: AccountingRules
    execution_lag_days: int
    cash_policy: str


def run_backtest(
    *,
    snapshot_root: Path,
    spec: dict[str, Any],
    snapshot_storage_key: str,
    snapshot_checksum: str,
    document_cutoff_at: str,
    parameters: dict[str, Any] | None = None,
    signals: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    path = snapshot_root / snapshot_storage_key
    if not path.is_file():
        raise BacktestError("snapshot artifact was not found")
    raw = path.read_bytes()
    if hashlib.sha256(raw).hexdigest() != snapshot_checksum:
        raise BacktestError("snapshot checksum mismatch; refusing to run on altered data")

    panel = load_panel(raw)
    result = simulate(panel, spec, parameters or {}, signals or [])

    engine_version = result["engine_version"]
    summary = dict(result["metrics"])
    summary["engine_version"] = engine_version
    # ENGINE_VERSION names the simulation math; these name the environment that
    # executed it. Floating-point accumulation can move between polars releases
    # under unchanged math, so a result_checksum only identifies a run when the
    # interpreter and the dataframe library are recorded next to it. Both are
    # pinned by requirements.txt; this is where a stored run says which pins.
    summary["python_version"] = platform.python_version()
    summary["polars_version"] = pl.__version__
    summary["document_cutoff_at"] = document_cutoff_at
    summary["n_rebalances"] = result["n_rebalances"]
    summary["n_trading_days"] = result["n_trading_days"]
    # How many symbols the committed spec authorized and the snapshot could
    # price. Anything the snapshot carried beyond the universe was ignored.
    summary["n_universe_symbols"] = result["n_universe_symbols"]
    # Enough to tell whether research actually moved this run, without reopening
    # the artifact: how many signals were supplied, how many rebalances saw one
    # live, and the weight they were applied at.
    summary["n_claim_signals"] = result["n_claim_signals"]
    summary["n_claim_supported_rebalances"] = result["n_claim_supported_rebalances"]
    summary["claim_signal_weight"] = result["claim_signal_weight"]
    summary["require_claim_support"] = result["require_claim_support"]
    # The accounting this run was simulated under. `engine_version` alone selects
    # all of it, but naming each rule means a stored summary explains its own
    # numbers without a reader having to look up what that version decided.
    summary["position_tracking"] = result["position_tracking"]
    summary["momentum_scale"] = result["momentum_scale"]
    summary["execution_lag_days"] = result["execution_lag_days"]
    summary["cash_policy"] = result["cash_policy"]
    summary["n_stop_loss_exits"] = result["n_stop_loss_exits"]

    # The worker writes these exact bytes as the run artifact, so the checksum
    # taken here is also the checksum of the stored file.
    curve = equity_curve_csv(result)
    return {
        "summary": summary,
        "equity_curve_csv": curve,
        "checksum": _result_checksum(curve),
        "engine_version": engine_version,
        # The last rebalance is the paper portfolio this run proposes. It does
        # not enter the checksum: the artifact is the equity curve, and these
        # are a projection of the same simulation for Go to persist.
        "candidates": result["candidates"],
    }


def load_panel(raw: bytes) -> pl.DataFrame:
    """Parse the frozen snapshot CSV into a (date, symbol, close) panel.

    ``adj_close`` is the total-return price; fall back to ``close`` if absent.
    """

    frame = pl.read_csv(io.BytesIO(raw), try_parse_dates=True)
    price = "adj_close" if "adj_close" in frame.columns else "close"
    return frame.select(
        pl.col("date").cast(pl.Date),
        pl.col("symbol").cast(pl.Utf8),
        pl.col(price).cast(pl.Float64).alias("close"),
    ).sort(["symbol", "date"])


def simulate(
    panel: pl.DataFrame,
    spec: dict[str, Any],
    parameters: dict[str, Any],
    signals: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    params = _engine_params(spec, parameters)
    lookback_days = params.lookback_months * _TRADING_DAYS_PER_MONTH
    # The committed spec decides what may be traded, not whatever the snapshot
    # happens to contain.
    universe = _universe_symbols(spec)
    claim_signals = _normalize_signals(
        signals or [], params.claim_horizon_days, params.min_claim_confidence, universe
    )

    wide = panel.pivot(values="close", index="date", on="symbol").sort("date")
    dates: list[dt.date] = wide.get_column("date").to_list()
    _require_universe_coverage(universe, wide.columns)
    symbols = universe
    series = {symbol: wide.get_column(symbol).to_list() for symbol in symbols}
    top_n = params.top_n if params.top_n is not None else min(3, len(symbols))

    schedule = spec["rebalance"]["schedule"]
    rebalance_set = set(_rebalance_indices(dates, schedule, lookback_days))

    simulator = (
        _simulate_drift
        if params.rules.position_tracking == "drift"
        else _simulate_constant_weights
    )
    run = simulator(
        dates=dates,
        series=series,
        symbols=symbols,
        rebalance_set=rebalance_set,
        lookback_days=lookback_days,
        top_n=top_n,
        params=params,
        claim_signals=claim_signals,
    )

    metrics = _metrics(
        run["equity_curve"], run["daily_returns"], run["total_turnover"], run["invested_fraction"]
    )
    return {
        "equity_curve": run["equity_curve"],
        "holdings": run["holdings"],
        "daily_returns": run["daily_returns"],
        "metrics": metrics,
        "candidates": run["candidates"],
        "n_rebalances": run["n_rebalances"],
        "n_stop_loss_exits": run["n_stop_loss_exits"],
        "n_trading_days": len(dates),
        "n_universe_symbols": len(symbols),
        "n_claim_signals": len(claim_signals),
        "n_claim_supported_rebalances": run["n_claim_supported_rebalances"],
        "claim_signal_weight": params.signal_weight,
        "require_claim_support": params.require_claim_support,
        "engine_version": params.engine_version,
        "position_tracking": params.rules.position_tracking,
        "momentum_scale": params.rules.momentum_scale,
        "execution_lag_days": params.execution_lag_days,
        "cash_policy": params.cash_policy,
    }


def _simulate_drift(
    *,
    dates: list[dt.date],
    series: dict[str, list[float | None]],
    symbols: list[str],
    rebalance_set: set[int],
    lookback_days: int,
    top_n: int,
    params: EngineParams,
    claim_signals: list[ClaimSignal],
) -> dict[str, Any]:
    """Simulate a book whose positions drift with prices between fills.

    Holdings are carried as market values, so a position's weight moves with its
    price and turnover is only charged when an order actually fills. Every trade
    — a scheduled rebalance or a stop-loss exit — is decided on one close and
    filled ``execution_lag_days`` later, so no decision can use the bar it
    trades on.
    """

    cost_rate = params.cost_bps / 10_000

    equity = 1.0
    cash = 1.0
    values: dict[str, float] = {}
    entry_price: dict[str, float] = {}
    pending: list[Order] = []
    exit_pending: set[str] = set()

    equity_curve: list[tuple[dt.date, float]] = []
    daily_returns: list[float] = []
    holdings_log: list[dict[str, Any]] = []
    invested: list[float] = []
    total_turnover = 0.0
    n_rebalances = 0
    n_stop_exits = 0
    n_claim_supported_rebalances = 0
    invested_from: int | None = None
    last_rebalance: dict[str, Any] = {"as_of": None, "positions": []}

    for index, day in enumerate(dates):
        # 1. Mark the book to today's close. A symbol the snapshot cannot price
        #    today holds its value, which is how v3 treated a missing bar too.
        if index > 0:
            previous_equity = equity
            for symbol in values:
                previous = series[symbol][index - 1]
                current = series[symbol][index]
                if previous is not None and current is not None and previous > 0:
                    values[symbol] *= current / previous
            equity = cash + math.fsum(values.values())
            daily_returns.append(equity / previous_equity - 1 if previous_equity > 0 else 0.0)

        # 2. A stop loss is a standing instruction: it reads today's close and
        #    sends an order, which fills on the lagged close like any other. One
        #    exit per position stays in flight, so a position that keeps falling
        #    is not sold repeatedly.
        if params.stop_loss is not None:
            for symbol in list(values):
                if symbol in exit_pending:
                    continue
                current = series[symbol][index]
                entry = entry_price.get(symbol)
                if current is not None and entry and (current / entry - 1) <= -params.stop_loss:
                    pending.append(
                        Order(index + params.execution_lag_days, "stop_exit", {symbol: 0.0}, None)
                    )
                    exit_pending.add(symbol)

        # 3. Today's scheduled decision, taken on data through today's close.
        #    Only claims already effective today are visible, so a later claim
        #    can never change an earlier holding.
        if index in rebalance_set:
            active_claims = _active_claims(claim_signals, day)
            claim_scores = _claim_scores(active_claims)
            positions = _select(series, symbols, index, lookback_days, top_n, params, claim_scores)
            pending.append(
                Order(
                    index + params.execution_lag_days,
                    "rebalance",
                    {position.symbol: position.weight for position in positions},
                    Decision(day, positions, active_claims, bool(claim_scores)),
                )
            )

        # 4. Fill everything due at today's close, in the order it was decided.
        #    Every order carries the same lag, so decision order is fill order:
        #    an exit decided before a rebalance has already filled by the time
        #    that rebalance does, and one decided after it — on a drawdown the
        #    rebalance could not have seen — still gets to act.
        if pending:
            due = [order for order in pending if order.execute_at <= index]
            pending = [order for order in pending if order.execute_at > index]
            for order in due:
                if order.kind == "stop_exit":
                    symbol = next(iter(order.weights))
                    exit_pending.discard(symbol)
                    held = values.get(symbol, 0.0)
                    if held <= 0 or equity <= 0:
                        continue
                    cost = cost_rate * held
                    total_turnover += held / equity
                    del values[symbol]
                    entry_price.pop(symbol, None)
                    cash += held - cost
                    equity -= cost
                    n_stop_exits += 1
                    continue

                equity, cash, values, turnover = _apply_target(
                    values, cash, equity, order.weights, cost_rate
                )
                total_turnover += turnover
                n_rebalances += 1
                # Entry prices are the prices actually paid, so a stop loss
                # measures its drawdown from the fill rather than the decision.
                entry_price = {
                    symbol: price
                    for symbol in values
                    if (price := series[symbol][index]) is not None
                }
                decision = order.decision
                if decision is not None:
                    if decision.had_claim_support:
                        n_claim_supported_rebalances += 1
                    holdings_log.append(
                        {
                            "decided_at": decision.as_of.isoformat(),
                            "date": day.isoformat(),
                            "weights": {
                                position.symbol: round(position.weight, 8)
                                for position in decision.positions
                            },
                            # The research support behind each holding, as of the
                            # day it was decided.
                            "claim_support": {
                                position.symbol: round(position.claim_support, 8)
                                for position in decision.positions
                            },
                        }
                    )
                    # The most recent filled rebalance is the paper portfolio the
                    # run proposes.
                    last_rebalance = _candidate_set(
                        decision.as_of, decision.positions, decision.active_claims
                    )
                if invested_from is None:
                    invested_from = index

        # 5. Record how much of the book was actually at risk. Measured from the
        #    first fill, because the lookback window before it is cash by
        #    construction and would only dilute the number.
        if invested_from is not None:
            invested.append(math.fsum(values.values()) / equity if equity > 0 else 0.0)
        equity_curve.append((day, equity))

    return {
        "equity_curve": equity_curve,
        "holdings": holdings_log,
        "daily_returns": daily_returns,
        "candidates": last_rebalance,
        "total_turnover": total_turnover,
        "n_rebalances": n_rebalances,
        "n_stop_loss_exits": n_stop_exits,
        "n_claim_supported_rebalances": n_claim_supported_rebalances,
        "invested_fraction": statistics.fmean(invested) if invested else 0.0,
    }


def _apply_target(
    values: dict[str, float],
    cash: float,
    equity: float,
    target: dict[str, float],
    cost_rate: float,
) -> tuple[float, float, dict[str, float], float]:
    """Trade the book to ``target`` weights, charging the value actually traded.

    Turnover is the traded value as a fraction of equity, so the cost works out
    to the same ``cost_bps / 10_000 * turnover`` v3 charged — the difference is
    that the position it is measured against has been marked to market rather
    than assumed to still sit at its last target.
    """

    if equity <= 0:
        return equity, cash, {}, 0.0

    # fsum is exact, so neither the turnover nor the resulting cash can depend on
    # the order this set happened to iterate in.
    traded_value = math.fsum(
        abs(target.get(symbol, 0.0) * equity - values.get(symbol, 0.0))
        for symbol in set(target) | set(values)
    )
    equity_after = equity - cost_rate * traded_value
    new_values = {symbol: weight * equity_after for symbol, weight in target.items() if weight > 0}
    return equity_after, equity_after - math.fsum(new_values.values()), new_values, traded_value / equity


def _simulate_constant_weights(
    *,
    dates: list[dt.date],
    series: dict[str, list[float | None]],
    symbols: list[str],
    rebalance_set: set[int],
    lookback_days: int,
    top_n: int,
    params: EngineParams,
    claim_signals: list[ClaimSignal],
) -> dict[str, Any]:
    """The ``momentum-claims-v3`` accounting, preserved to reproduce prior runs.

    FROZEN. This exists so a strategy that ran under v3 can be re-run and its
    curve diffed against v4's, which only works while the arithmetic here stays
    bit-for-bit identical. Do not refactor it, do not share code into it, and do
    not fix its accounting — the defects are the point. `_simulate_drift` is
    where corrections go.

    Two of those defects are visible right here: target weights are reapplied to
    every daily return while turnover is charged only on rebalance dates, and the
    stop loss zeroes a weight without paying for the sale.
    """

    equity = 1.0
    equity_curve: list[tuple[dt.date, float]] = []
    holdings_log: list[dict[str, Any]] = []
    daily_returns: list[float] = []
    invested: list[float] = []
    weights: dict[str, float] = {}
    # v3 recorded whatever the snapshot held on the rebalance bar, including a
    # missing one. A None entry reads as falsy below, which is how the stop loss
    # skipped an unpriced position; typing it honestly keeps that visible rather
    # than asserting a price that was never there.
    entry_price: dict[str, float | None] = {}
    total_turnover = 0.0
    n_rebalances = 0
    n_stop_exits = 0
    n_claim_supported_rebalances = 0
    invested_from: int | None = None
    last_rebalance: dict[str, Any] = {"as_of": None, "positions": []}

    for index, day in enumerate(dates):
        if index > 0:
            day_return = 0.0
            for symbol, weight in weights.items():
                previous = series[symbol][index - 1]
                current = series[symbol][index]
                if previous is not None and current is not None and previous > 0:
                    day_return += weight * (current / previous - 1)
            equity *= 1 + day_return
            daily_returns.append(day_return)

        if params.stop_loss is not None:
            for symbol in list(weights):
                current = series[symbol][index]
                entry = entry_price.get(symbol)
                if current is not None and entry and (current / entry - 1) <= -params.stop_loss:
                    if weights[symbol] > 0:
                        n_stop_exits += 1
                    weights[symbol] = 0.0

        if index in rebalance_set:
            active_claims = _active_claims(claim_signals, day)
            claim_scores = _claim_scores(active_claims)
            if claim_scores:
                n_claim_supported_rebalances += 1
            positions = _select(series, symbols, index, lookback_days, top_n, params, claim_scores)
            target = {position.symbol: position.weight for position in positions}
            turnover = sum(
                abs(target.get(symbol, 0.0) - weights.get(symbol, 0.0))
                for symbol in set(target) | set(weights)
            )
            equity *= 1 - params.cost_bps / 10_000 * turnover
            total_turnover += turnover
            n_rebalances += 1
            weights = {symbol: weight for symbol, weight in target.items() if weight > 0}
            entry_price = {symbol: series[symbol][index] for symbol in weights}
            holdings_log.append(
                {
                    "decided_at": day.isoformat(),
                    "date": day.isoformat(),
                    "weights": {symbol: round(weight, 8) for symbol, weight in weights.items()},
                    # The research support behind each holding, as of this day.
                    "claim_support": {
                        symbol: round(claim_scores.get(symbol, 0.0), 8) for symbol in weights
                    },
                }
            )
            # The most recent rebalance is the paper portfolio the run proposes.
            last_rebalance = _candidate_set(day, positions, active_claims)
            if invested_from is None:
                invested_from = index

        if invested_from is not None:
            invested.append(sum(weights.values()))
        equity_curve.append((day, equity))

    return {
        "equity_curve": equity_curve,
        "holdings": holdings_log,
        "daily_returns": daily_returns,
        "candidates": last_rebalance,
        "total_turnover": total_turnover,
        "n_rebalances": n_rebalances,
        "n_stop_loss_exits": n_stop_exits,
        "n_claim_supported_rebalances": n_claim_supported_rebalances,
        "invested_fraction": statistics.fmean(invested) if invested else 0.0,
    }


def _universe_symbols(spec: dict[str, Any]) -> list[str]:
    """The symbols the committed spec authorizes this strategy to trade.

    Validated here rather than trusted, because this service is an independent
    HTTP boundary: it must not rank a symbol just because a caller managed to get
    it into the snapshot. Returned sorted and deduplicated so the ranking order
    cannot depend on how the spec happened to list them.
    """

    universe = spec.get("universe")
    if not isinstance(universe, dict):
        raise BacktestError("spec is missing its universe")
    raw = universe.get("symbols")
    if not isinstance(raw, list) or not raw:
        raise BacktestError("spec universe must list at least one symbol")

    symbols: set[str] = set()
    for item in raw:
        symbol = str(item).strip()
        if not symbol:
            raise BacktestError("spec universe contains an empty symbol")
        symbols.add(symbol)
    return sorted(symbols)


def _require_universe_coverage(universe: list[str], columns: list[str]) -> None:
    """Refuse to run when the snapshot cannot price the whole universe.

    Silently trading the subset that happens to be present would make the run's
    result depend on data availability rather than on the reviewed spec, so a gap
    is an error the caller has to resolve by fetching the right snapshot.
    """

    available = {column for column in columns if column != "date"}
    missing = [symbol for symbol in universe if symbol not in available]
    if missing:
        raise BacktestError(
            "snapshot does not cover the strategy universe; missing: " + ", ".join(missing)
        )


def _candidate_set(
    day: dt.date,
    positions: list[Position],
    active_claims: dict[str, list[ClaimSignal]],
) -> dict[str, Any]:
    """Rank a rebalance's positions and attribute each to the claims behind it.

    ``as_of`` is the day the rebalance was *decided*, not the day it filled: it
    says which information produced the ranking, which is what pairs it with the
    run's document cutoff.

    ``contribution`` is a claim's own signed confidence. Contributions need not
    sum to ``claim_support``, which is the net clamped to [-1, 1].
    """

    return {
        "as_of": day.isoformat(),
        "positions": [
            {
                "symbol": position.symbol,
                "rank": rank,
                "weight": round(position.weight, 8),
                "score": round(position.score, 8),
                "momentum": round(position.momentum, 8),
                "momentum_rank": round(position.momentum_rank, 8),
                "claim_support": round(position.claim_support, 8),
                "evidence": [
                    {"claim_id": signal.claim_id, "contribution": round(signal.score, 8)}
                    for signal in active_claims.get(position.symbol, [])
                ],
            }
            for rank, position in enumerate(positions, start=1)
        ],
    }


def _engine_params(spec: dict[str, Any], parameters: dict[str, Any]) -> EngineParams:
    # The accounting is chosen first, because it supplies the defaults that the
    # spec and the run parameters then override.
    engine_version = str(parameters.get("engine_version") or ENGINE_VERSION)
    rules = _ACCOUNTING.get(engine_version)
    if rules is None:
        raise BacktestError(
            f"unsupported engine_version {engine_version!r}; expected one of "
            + ", ".join(sorted(_ACCOUNTING))
        )

    lookback_months = 6
    top_n: int | None = None
    gates: list[tuple[str, float]] = []
    signal_weight = rules.signal_weight
    require_claim_support = False
    claim_horizon_days = _DEFAULT_CLAIM_HORIZON_DAYS
    min_claim_confidence = 0.0
    execution_lag_days = rules.execution_lag_days
    cash_policy = "cash"

    for filter_spec in spec.get("selection", {}).get("filters", []):
        field = filter_spec.get("field")
        operator = filter_spec.get("operator")
        value = filter_spec.get("value")
        if field == "lookback_months" and operator == "eq":
            lookback_months = int(value)
        elif field == "top_n" and operator == "eq":
            top_n = int(value)
        elif field == "claim_confidence" and operator in {"gt", "gte"}:
            min_claim_confidence = float(value)
        elif field == "claim_signal_weight" and operator == "eq":
            signal_weight = float(value)
        elif field == "require_claim_support" and operator == "eq":
            require_claim_support = bool(value)
        elif field == "claim_horizon_days" and operator == "eq":
            claim_horizon_days = int(value)
        elif field == "execution_lag_days" and operator == "eq":
            execution_lag_days = int(value)
        elif field == "cash_policy" and operator == "eq":
            cash_policy = str(value)
        elif field == "momentum" and operator in {"gt", "gte", "lt", "lte"}:
            gates.append((operator, float(value)))

    # Run parameters override the committed spec so a strategy version can be
    # re-tested at a different tilt without minting a new version.
    if "lookback_months" in parameters:
        lookback_months = int(parameters["lookback_months"])
    if "top_n" in parameters:
        top_n = int(parameters["top_n"])
    if "claim_signal_weight" in parameters:
        signal_weight = float(parameters["claim_signal_weight"])
    if "require_claim_support" in parameters:
        require_claim_support = bool(parameters["require_claim_support"])
    if "claim_horizon_days" in parameters:
        claim_horizon_days = int(parameters["claim_horizon_days"])
    if "claim_confidence" in parameters:
        min_claim_confidence = float(parameters["claim_confidence"])
    if "execution_lag_days" in parameters:
        execution_lag_days = int(parameters["execution_lag_days"])
    if "cash_policy" in parameters:
        cash_policy = str(parameters["cash_policy"])

    if lookback_months < 1:
        raise BacktestError("lookback_months must be >= 1")
    if top_n is not None and top_n < 1:
        raise BacktestError("top_n must be >= 1")
    if signal_weight < 0 or signal_weight > 5:
        raise BacktestError("claim_signal_weight must be in [0, 5]")
    if claim_horizon_days < 1:
        raise BacktestError("claim_horizon_days must be >= 1")
    if not 0.0 <= min_claim_confidence <= 1.0:
        raise BacktestError("claim_confidence must be in [0, 1]")
    if not 0 <= execution_lag_days <= _MAX_EXECUTION_LAG_DAYS:
        raise BacktestError(f"execution_lag_days must be in [0, {_MAX_EXECUTION_LAG_DAYS}]")
    if cash_policy not in _CASH_POLICIES:
        raise BacktestError("cash_policy must be one of " + ", ".join(_CASH_POLICIES))

    # The previous accounting is kept to reproduce runs made under it, so it
    # accepts only the configuration it could actually have run. Silently
    # applying a v4 knob to it would produce a curve that never existed.
    if engine_version != ENGINE_VERSION:
        if execution_lag_days != rules.execution_lag_days:
            raise BacktestError(
                f"execution_lag_days requires engine_version {ENGINE_VERSION!r}; "
                f"{engine_version!r} filled at the deciding bar"
            )
        if cash_policy != "cash":
            raise BacktestError(
                f"cash_policy requires engine_version {ENGINE_VERSION!r}; "
                f"{engine_version!r} always left the weight remainder in cash"
            )

    risk = spec["risk"]
    return EngineParams(
        lookback_months=lookback_months,
        top_n=top_n,
        gates=gates,
        max_weight=float(risk["max_position_weight"]),
        cost_bps=float(risk["transaction_cost_bps"]),
        stop_loss=float(risk["stop_loss_pct"]) if risk.get("stop_loss_pct") is not None else None,
        signal_weight=signal_weight,
        require_claim_support=require_claim_support,
        claim_horizon_days=claim_horizon_days,
        min_claim_confidence=min_claim_confidence,
        engine_version=engine_version,
        rules=rules,
        execution_lag_days=execution_lag_days,
        cash_policy=cash_policy,
    )


def _normalize_signals(
    raw: list[dict[str, Any]],
    default_horizon_days: int,
    min_confidence: float,
    universe: list[str],
) -> list[ClaimSignal]:
    """Resolve wire signals into date windows, sorted on a total key.

    Sorting here rather than trusting the caller's order is what makes the
    floating-point accumulation in `_claim_scores` reproducible.

    A signal for a symbol outside the universe is dropped, not rejected: a claim
    may legitimately cite something this strategy does not trade, and it stays
    evidence for the strategy without becoming a trading signal. Dropping it here
    also keeps ``n_claim_signals`` honest about what could actually have moved
    the run.
    """

    tradable = set(universe)
    signals: list[ClaimSignal] = []
    for item in raw:
        direction = str(item.get("direction", "neutral"))
        sign = {"positive": 1.0, "negative": -1.0}.get(direction)
        if sign is None:
            continue
        symbol = str(item.get("symbol", "")).strip()
        if not symbol:
            raise BacktestError("claim signal is missing a symbol")
        if symbol not in tradable:
            continue
        confidence = float(item.get("confidence", 0.0))
        if not 0.0 <= confidence <= 1.0:
            raise BacktestError("claim signal confidence must be in [0, 1]")
        if confidence < min_confidence:
            continue
        horizon = int(item.get("horizon_days") or default_horizon_days)
        if horizon < 1:
            raise BacktestError("claim signal horizon_days must be >= 1")
        effective = _as_date(item.get("effective_at"))
        signals.append(
            ClaimSignal(
                claim_id=str(item.get("claim_id", "")),
                symbol=symbol,
                score=sign * confidence,
                effective=effective,
                expires=effective + dt.timedelta(days=horizon),
            )
        )

    signals.sort(key=lambda signal: (signal.symbol, signal.effective, signal.claim_id, signal.score))
    return signals


def _as_date(value: Any) -> dt.date:
    """Normalize a timestamp to its UTC calendar date."""

    if isinstance(value, dt.datetime):
        moment = value
    else:
        text = str(value or "").strip()
        try:
            moment = dt.datetime.fromisoformat(text.replace("Z", "+00:00"))
        except ValueError as error:
            raise BacktestError(f"invalid claim signal timestamp {value!r}") from error
    if moment.tzinfo is not None:
        moment = moment.astimezone(dt.timezone.utc)
    return moment.date()


def _active_claims(signals: list[ClaimSignal], day: dt.date) -> dict[str, list[ClaimSignal]]:
    """Claims live on ``day``, grouped by symbol in the engine's sorted order."""

    active: dict[str, list[ClaimSignal]] = {}
    for signal in signals:
        if signal.effective <= day < signal.expires:
            active.setdefault(signal.symbol, []).append(signal)
    return active


def _claim_scores(active: dict[str, list[ClaimSignal]]) -> dict[str, float]:
    """Net signed confidence per symbol, clamped so one repeated view cannot
    dominate momentum outright."""

    return {
        symbol: max(-1.0, min(1.0, sum(signal.score for signal in signals)))
        for symbol, signals in active.items()
    }


def _rebalance_indices(dates: list[dt.date], schedule: str, min_index: int) -> list[int]:
    picked: list[int] = []
    last_key: Any = None
    for index in range(min_index, len(dates)):
        day = dates[index]
        if schedule == "weekly":
            key = day.isocalendar()[:2]
        elif schedule == "monthly":
            key = (day.year, day.month)
        elif schedule == "quarterly":
            key = (day.year, (day.month - 1) // 3)
        else:
            raise BacktestError(f"unsupported rebalance schedule {schedule!r}")
        if key != last_key:
            picked.append(index)
            last_key = key
    return picked


def _select(
    series: dict[str, list[float | None]],
    symbols: list[str],
    index: int,
    lookback_days: int,
    top_n: int,
    params: EngineParams,
    claim_scores: dict[str, float],
) -> list[Position]:
    eligible: list[tuple[str, float, float]] = []
    for symbol in symbols:
        current = series[symbol][index]
        past = series[symbol][index - lookback_days] if index - lookback_days >= 0 else None
        if current is None or past is None or past <= 0:
            continue
        momentum = current / past - 1
        # Gates read the raw trailing return, because that is what a reviewer
        # committing "momentum > 0.05" meant. Only the ranking is normalized.
        if not _passes_gates(momentum, params.gates):
            continue
        support = claim_scores.get(symbol, 0.0)
        if params.require_claim_support and support <= 0:
            continue
        eligible.append((symbol, momentum, support))

    if not eligible:
        return []

    if params.rules.momentum_scale == "rank":
        ranked = _momentum_ranks(eligible)
    else:
        ranked = {symbol: momentum for symbol, momentum, _ in eligible}

    scored = [
        (symbol, ranked[symbol] + params.signal_weight * support, momentum, ranked[symbol], support)
        for symbol, momentum, support in eligible
    ]
    # Score descending, then symbol ascending so ties are deterministic.
    scored.sort(key=lambda item: (-item[1], item[0]))

    chosen = scored[: _position_count(len(scored), top_n, params)]
    if not chosen:
        return []
    weight = min(1.0 / len(chosen), params.max_weight)
    return [
        Position(
            symbol=symbol,
            weight=weight,
            score=score,
            momentum=momentum,
            momentum_rank=momentum_rank,
            claim_support=support,
        )
        for symbol, score, momentum, momentum_rank, support in chosen
    ]


def _momentum_ranks(eligible: list[tuple[str, float, float]]) -> dict[str, float]:
    """Spread trailing returns evenly over [-1, 1] by cross-sectional rank.

    Ranking in return space adds a confidence to a return, so a claim's real
    influence drifts with each asset's volatility — the same
    ``claim_signal_weight`` reorders a quiet universe and does nothing to a
    violent one. Rank space gives the weight one meaning everywhere: the
    fraction of the momentum spread a full-confidence claim is worth.

    Tied returns share the average rank, so the tilt cannot turn on symbol names.
    """

    count = len(eligible)
    if count == 1:
        return {eligible[0][0]: 0.0}

    ordered = sorted(eligible, key=lambda item: (item[1], item[0]))
    ranks: dict[str, float] = {}
    start = 0
    while start < count:
        stop = start + 1
        while stop < count and ordered[stop][1] == ordered[start][1]:
            stop += 1
        shared = 2.0 * ((start + stop - 1) / 2.0) / (count - 1) - 1.0
        for position in range(start, stop):
            ranks[ordered[position][0]] = shared
        start = stop
    return ranks


def _position_count(n_eligible: int, top_n: int, params: EngineParams) -> int:
    """How many names to hold, given the cap and the cash policy.

    ``min(1/n, max_position_weight)`` caps a position without redistributing, so
    a ``max_position_weight`` below ``1/top_n`` leaves the rest of the book in
    cash. Under ``cash`` that is the answer and ``invested_fraction`` reports it.
    Under ``extend`` the cap decides the count instead: hold as many ranked names
    as being fully invested requires, which respects the cap rather than
    quietly breaching it. An eligible set too small to fill the book still runs
    partly in cash — that is the universe's doing, and the summary says so.
    """

    count = min(n_eligible, top_n)
    if params.cash_policy != "extend" or count == 0:
        return count
    # Guarded against float noise: 1/0.2 must give 5 names, not 6.
    needed = math.ceil(1.0 / params.max_weight - 1e-9)
    return min(n_eligible, max(count, needed))


def _passes_gates(momentum: float, gates: list[tuple[str, float]]) -> bool:
    for operator, threshold in gates:
        if operator == "gt" and not momentum > threshold:
            return False
        if operator == "gte" and not momentum >= threshold:
            return False
        if operator == "lt" and not momentum < threshold:
            return False
        if operator == "lte" and not momentum <= threshold:
            return False
    return True


def _metrics(
    equity_curve: list[tuple[dt.date, float]],
    daily_returns: list[float],
    total_turnover: float,
    invested_fraction: float,
) -> dict[str, float]:
    final_equity = equity_curve[-1][1] if equity_curve else 1.0
    total_return = final_equity - 1.0

    if daily_returns and final_equity > 0:
        cagr = final_equity ** (_TRADING_DAYS_PER_YEAR / len(daily_returns)) - 1
    else:
        cagr = 0.0

    if len(daily_returns) > 1:
        deviation = statistics.stdev(daily_returns)
        volatility = deviation * math.sqrt(_TRADING_DAYS_PER_YEAR)
        mean_return = statistics.mean(daily_returns)
        sharpe = (mean_return / deviation * math.sqrt(_TRADING_DAYS_PER_YEAR)) if deviation > 0 else 0.0
    else:
        volatility = 0.0
        sharpe = 0.0

    peak = float("-inf")
    max_drawdown = 0.0
    for _, value in equity_curve:
        peak = max(peak, value)
        if peak > 0:
            max_drawdown = min(max_drawdown, value / peak - 1)

    return {
        "final_equity": round(final_equity, 6),
        "total_return": round(total_return, 6),
        "cagr": round(cagr, 6),
        "annualized_volatility": round(volatility, 6),
        "sharpe": round(sharpe, 6),
        "max_drawdown": round(max_drawdown, 6),
        "total_turnover": round(total_turnover, 6),
        # Mean share of equity actually held in positions, from the first fill
        # onward. A cap below 1/top_n runs the rest of the book as cash, which
        # otherwise looks like a strategy that simply does not move much.
        "invested_fraction": round(invested_fraction, 6),
    }


def equity_curve_csv(result: dict[str, Any]) -> str:
    """Serialize the equity curve deterministically (fixed precision, date order)."""

    lines = ["date,equity"]
    lines.extend(f"{day.isoformat()},{value:.8f}" for day, value in result["equity_curve"])
    return "\n".join(lines) + "\n"


def _result_checksum(curve: str) -> str:
    return hashlib.sha256(curve.encode("utf-8")).hexdigest()
