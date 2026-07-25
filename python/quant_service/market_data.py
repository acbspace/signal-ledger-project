"""Market data providers returning normalized, deterministic daily bars.

Providers fetch raw records; ``normalize_bars`` produces the canonical sorted
list the Go side serializes and checksums. Keeping normalization pure makes the
determinism testable without network access.
"""

from __future__ import annotations

import math
from datetime import date, timedelta
from typing import Any, Protocol

from pydantic import BaseModel


class MarketDataUnavailable(RuntimeError):
    """The provider failed transiently. Surfaced as a 502 so the job retries."""


class UnknownProviderError(ValueError):
    """MARKET_DATA_PROVIDER names a provider this service does not implement."""


class Bar(BaseModel):
    symbol: str
    date: str
    open: float
    high: float
    low: float
    close: float
    adj_close: float
    volume: int


class MarketDataProvider(Protocol):
    name: str

    def fetch_daily_bars(
        self, symbols: list[str], start_date: date, end_date: date
    ) -> list[dict[str, Any]]: ...


class YFinanceProvider:
    name = "yfinance"

    def fetch_daily_bars(
        self, symbols: list[str], start_date: date, end_date: date
    ) -> list[dict[str, Any]]:
        import yfinance

        records: list[dict[str, Any]] = []
        for symbol in symbols:
            try:
                frame = yfinance.Ticker(symbol).history(
                    start=start_date.isoformat(),
                    # yfinance treats end as exclusive; the request is inclusive.
                    end=(end_date + timedelta(days=1)).isoformat(),
                    interval="1d",
                    auto_adjust=False,
                    actions=False,
                )
            except Exception as error:
                raise MarketDataUnavailable(f"yfinance fetch failed for {symbol}") from error

            for timestamp, row in frame.iterrows():
                records.append(
                    {
                        "symbol": symbol,
                        "date": timestamp.date().isoformat(),
                        "open": row.get("Open"),
                        "high": row.get("High"),
                        "low": row.get("Low"),
                        "close": row.get("Close"),
                        "adj_close": row.get("Adj Close", row.get("Close")),
                        "volume": row.get("Volume"),
                    }
                )
        return records


def get_provider(name: str) -> MarketDataProvider:
    if name == "yfinance":
        return YFinanceProvider()
    raise UnknownProviderError(name)


def normalize_bars(records: list[dict[str, Any]]) -> list[Bar]:
    """Drop incomplete rows and sort by (symbol, date) for determinism."""

    bars: list[Bar] = []
    for record in records:
        # Annotated rather than inferred: the guard below is what rejects a
        # missing price, and mypy cannot narrow `Any | None` through `any()`.
        prices: list[Any] = [
            record.get("open"),
            record.get("high"),
            record.get("low"),
            record.get("close"),
            record.get("adj_close"),
        ]
        if any(price is None or _is_nan(price) for price in prices):
            continue
        volume = record.get("volume")
        if volume is None or _is_nan(volume):
            continue
        bars.append(
            Bar(
                symbol=str(record["symbol"]),
                date=str(record["date"]),
                open=float(prices[0]),
                high=float(prices[1]),
                low=float(prices[2]),
                close=float(prices[3]),
                adj_close=float(prices[4]),
                volume=int(volume),
            )
        )
    bars.sort(key=lambda bar: (bar.symbol, bar.date))
    return bars


def _is_nan(value: Any) -> bool:
    try:
        return math.isnan(float(value))
    except (TypeError, ValueError):
        return True
