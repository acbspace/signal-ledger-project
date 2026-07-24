"""Deterministic backtest over price momentum tilted by point-in-time research.

Selection ranks the universe by trailing-return momentum and then tilts that rank
by the research claims that were already effective on the rebalance day. Two
independent no-look-ahead rules hold: a price decision uses only bars at or
before that day, and a claim only counts from its ``effective_at`` until its
horizon expires. The Go worker additionally drops any claim effective after the
run's document cutoff, so the engine never even receives future research.

Given the same frozen snapshot, spec, and signals it produces byte-identical
results, so `run_backtest` verifies the snapshot checksum before simulating and
hashes the equity curve as the result checksum.

The snapshot is the canonical CSV frozen by the Go worker (columns:
symbol,date,open,high,low,close,adj_close,volume). This process stays stateless:
it reads the snapshot read-only and returns results for Go to persist.

`ENGINE_VERSION` must change whenever the simulation math changes.
"""

from __future__ import annotations

import datetime as dt
import hashlib
import io
import math
import statistics
from dataclasses import dataclass
from pathlib import Path
from typing import Any, NamedTuple

import polars as pl

ENGINE_VERSION = "momentum-claims-v2"
_TRADING_DAYS_PER_YEAR = 252
_TRADING_DAYS_PER_MONTH = 21

# A full-confidence claim moves a symbol's score by this much, expressed in the
# same units as trailing return: 0.1 means "worth ten points of momentum".
_DEFAULT_SIGNAL_WEIGHT = 0.1
# How long a claim stays live when it did not state its own horizon.
_DEFAULT_CLAIM_HORIZON_DAYS = 90


class BacktestError(ValueError):
    """The backtest cannot be run for the given inputs."""


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
    """One selected symbol at a rebalance, with the inputs that selected it."""

    symbol: str
    weight: float
    score: float
    momentum: float
    claim_support: float


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

    summary = dict(result["metrics"])
    summary["engine_version"] = ENGINE_VERSION
    summary["document_cutoff_at"] = document_cutoff_at
    summary["n_rebalances"] = result["n_rebalances"]
    summary["n_trading_days"] = result["n_trading_days"]
    # Enough to tell whether research actually moved this run, without reopening
    # the artifact: how many signals were supplied, how many rebalances saw one
    # live, and the weight they were applied at.
    summary["n_claim_signals"] = result["n_claim_signals"]
    summary["n_claim_supported_rebalances"] = result["n_claim_supported_rebalances"]
    summary["claim_signal_weight"] = result["claim_signal_weight"]
    summary["require_claim_support"] = result["require_claim_support"]

    # The worker writes these exact bytes as the run artifact, so the checksum
    # taken here is also the checksum of the stored file.
    curve = equity_curve_csv(result)
    return {
        "summary": summary,
        "equity_curve_csv": curve,
        "checksum": _result_checksum(curve),
        "engine_version": ENGINE_VERSION,
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
    claim_signals = _normalize_signals(signals or [], params.claim_horizon_days, params.min_claim_confidence)

    wide = panel.pivot(values="close", index="date", on="symbol").sort("date")
    dates: list[dt.date] = wide.get_column("date").to_list()
    symbols = sorted(column for column in wide.columns if column != "date")
    series = {symbol: wide.get_column(symbol).to_list() for symbol in symbols}
    top_n = params.top_n if params.top_n is not None else min(3, len(symbols))

    schedule = spec["rebalance"]["schedule"]
    rebalance_set = set(_rebalance_indices(dates, schedule, lookback_days))

    equity = 1.0
    equity_curve: list[tuple[dt.date, float]] = []
    holdings_log: list[dict[str, Any]] = []
    daily_returns: list[float] = []
    weights: dict[str, float] = {}
    entry_price: dict[str, float] = {}
    total_turnover = 0.0
    n_rebalances = 0
    n_claim_supported_rebalances = 0
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
                    weights[symbol] = 0.0

        if index in rebalance_set:
            # Only claims already effective on this day are visible here, so a
            # later claim can never change an earlier holding.
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

        equity_curve.append((day, equity))

    metrics = _metrics(equity_curve, daily_returns, total_turnover)
    return {
        "equity_curve": equity_curve,
        "holdings": holdings_log,
        "daily_returns": daily_returns,
        "metrics": metrics,
        "candidates": last_rebalance,
        "n_rebalances": n_rebalances,
        "n_trading_days": len(dates),
        "n_claim_signals": len(claim_signals),
        "n_claim_supported_rebalances": n_claim_supported_rebalances,
        "claim_signal_weight": params.signal_weight,
        "require_claim_support": params.require_claim_support,
    }


def _candidate_set(
    day: dt.date,
    positions: list[Position],
    active_claims: dict[str, list[ClaimSignal]],
) -> dict[str, Any]:
    """Rank a rebalance's positions and attribute each to the claims behind it.

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
    lookback_months = 6
    top_n: int | None = None
    gates: list[tuple[str, float]] = []
    signal_weight = _DEFAULT_SIGNAL_WEIGHT
    require_claim_support = False
    claim_horizon_days = _DEFAULT_CLAIM_HORIZON_DAYS
    min_claim_confidence = 0.0

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
    )


def _normalize_signals(
    raw: list[dict[str, Any]], default_horizon_days: int, min_confidence: float
) -> list[ClaimSignal]:
    """Resolve wire signals into date windows, sorted on a total key.

    Sorting here rather than trusting the caller's order is what makes the
    floating-point accumulation in `_claim_scores` reproducible.
    """

    signals: list[ClaimSignal] = []
    for item in raw:
        direction = str(item.get("direction", "neutral"))
        sign = {"positive": 1.0, "negative": -1.0}.get(direction)
        if sign is None:
            continue
        symbol = str(item.get("symbol", "")).strip()
        if not symbol:
            raise BacktestError("claim signal is missing a symbol")
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
    scored: list[tuple[str, float, float, float]] = []
    for symbol in symbols:
        current = series[symbol][index]
        past = series[symbol][index - lookback_days] if index - lookback_days >= 0 else None
        if current is None or past is None or past <= 0:
            continue
        momentum = current / past - 1
        if not _passes_gates(momentum, params.gates):
            continue
        support = claim_scores.get(symbol, 0.0)
        if params.require_claim_support and support <= 0:
            continue
        scored.append((symbol, momentum + params.signal_weight * support, momentum, support))

    # Score descending, then symbol ascending so ties are deterministic.
    scored.sort(key=lambda item: (-item[1], item[0]))
    chosen = scored[:top_n]
    if not chosen:
        return []
    weight = min(1.0 / len(chosen), params.max_weight)
    return [
        Position(symbol=symbol, weight=weight, score=score, momentum=momentum, claim_support=support)
        for symbol, score, momentum, support in chosen
    ]


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
    }


def equity_curve_csv(result: dict[str, Any]) -> str:
    """Serialize the equity curve deterministically (fixed precision, date order)."""

    lines = ["date,equity"]
    lines.extend(f"{day.isoformat()},{value:.8f}" for day, value in result["equity_curve"])
    return "\n".join(lines) + "\n"


def _result_checksum(curve: str) -> str:
    return hashlib.sha256(curve.encode("utf-8")).hexdigest()
