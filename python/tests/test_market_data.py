from datetime import date

from fastapi.testclient import TestClient

from quant_service import market_data
from quant_service.main import app


def test_normalize_bars_sorts_and_drops_incomplete_rows() -> None:
    records = [
        {"symbol": "XLE", "date": "2026-01-03", "open": 90.5, "high": 91, "low": 90, "close": 90.75, "adj_close": 90.75, "volume": 1200},
        {"symbol": "USO", "date": "2026-01-03", "open": 70.1, "high": 71, "low": 69.9, "close": 70.5, "adj_close": 70.5, "volume": 3400},
        {"symbol": "USO", "date": "2026-01-02", "open": float("nan"), "high": 70, "low": 68.5, "close": 69.75, "adj_close": 69.75, "volume": 3100},
    ]

    bars = market_data.normalize_bars(records)

    assert [(bar.symbol, bar.date) for bar in bars] == [
        ("USO", "2026-01-03"),
        ("XLE", "2026-01-03"),
    ]
    assert bars[0].volume == 3400


class FakeProvider:
    name = "fake"

    def fetch_daily_bars(self, symbols: list[str], start_date: date, end_date: date):
        return [
            {
                "symbol": symbol,
                "date": start_date.isoformat(),
                "open": 1.0,
                "high": 1.5,
                "low": 0.9,
                "close": 1.2,
                "adj_close": 1.2,
                "volume": 100,
            }
            for symbol in symbols
        ]


def test_market_data_endpoint_returns_normalized_bars(monkeypatch) -> None:
    monkeypatch.setattr(market_data, "get_provider", lambda name: FakeProvider())

    response = TestClient(app).post(
        "/v1/market-data",
        json={"symbols": ["USO", "XLE"], "start_date": "2026-01-02", "end_date": "2026-01-03"},
    )

    assert response.status_code == 200
    body = response.json()
    assert body["provider"] == "fake"
    assert [bar["symbol"] for bar in body["bars"]] == ["USO", "XLE"]
    assert body["bars"][0]["adj_close"] == 1.2


def test_market_data_endpoint_rejects_reversed_dates() -> None:
    response = TestClient(app).post(
        "/v1/market-data",
        json={"symbols": ["USO"], "start_date": "2026-02-01", "end_date": "2026-01-01"},
    )

    assert response.status_code == 422
