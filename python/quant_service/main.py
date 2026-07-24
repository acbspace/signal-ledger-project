"""HTTP boundary for market data, document extraction, and backtests.

The Go worker owns durable jobs and persistence. This process remains stateless:
it validates a request, executes a deterministic analysis task, and returns data
for Go to persist with the run metadata.
"""

import os
from datetime import date, datetime, timezone
from pathlib import Path
from typing import Any, Literal

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field

from quant_service import backtest, llm_extractor, market_data
from quant_service.extractor import ExtractionError, extract_pages, extract_pdf
from quant_service.llm_extractor import LLMExtractionError

app = FastAPI(
    title="SignalLedger Quant Service",
    version="0.1.0",
    description="Stateless OpenBB, PDF, and backtesting boundary.",
)


class MarketDataRequest(BaseModel):
    symbols: list[str] = Field(min_length=1)
    start_date: str
    end_date: str
    interval: Literal["1d"] = "1d"


class ClaimSignalInput(BaseModel):
    """One accepted claim the worker resolved to a tradable symbol.

    The worker has already applied the run's document cutoff; the engine applies
    ``effective_at`` and the horizon per rebalance.
    """

    claim_id: str
    symbol: str
    direction: Literal["positive", "negative", "neutral"]
    confidence: float = Field(ge=0, le=1)
    effective_at: datetime
    horizon_days: int | None = Field(default=None, ge=1)


class BacktestRequest(BaseModel):
    backtest_id: str
    spec: dict[str, Any]
    snapshot_storage_key: str
    snapshot_checksum: str
    document_cutoff_at: str
    parameters: dict[str, Any] = Field(default_factory=dict)
    signals: list[ClaimSignalInput] = Field(default_factory=list)


class CandidateEvidence(BaseModel):
    claim_id: str
    contribution: float


class CandidatePosition(BaseModel):
    symbol: str
    rank: int = Field(ge=1)
    weight: float
    score: float
    momentum: float
    claim_support: float
    evidence: list[CandidateEvidence] = Field(default_factory=list)


class CandidateSet(BaseModel):
    """The run's last rebalance: the paper portfolio it proposes, ranked."""

    as_of: date | None = None
    positions: list[CandidatePosition] = Field(default_factory=list)


class BacktestResponse(BaseModel):
    summary: dict[str, Any]
    equity_curve_csv: str
    checksum: str
    engine_version: str
    candidates: CandidateSet = Field(default_factory=CandidateSet)


class ClaimExtractionRequest(BaseModel):
    document_id: str
    storage_key: str
    effective_at: datetime


class ExtractedPage(BaseModel):
    page_number: int = Field(ge=1)
    content: str


class ExtractedClaim(BaseModel):
    page_number: int = Field(ge=1)
    ticker: str | None
    claim: str
    evidence_quote: str
    claim_kind: Literal["fundamental", "macro", "risk", "catalyst", "valuation"]
    direction: Literal["positive", "negative", "neutral"]
    horizon_days: int | None = Field(default=None, ge=1)
    confidence: float = Field(ge=0, le=1)
    effective_at: datetime


class ClaimExtractionResponse(BaseModel):
    pages: list[ExtractedPage]
    claims: list[ExtractedClaim]
    extractor: str


class MarketDataResponse(BaseModel):
    provider: str
    interval: Literal["1d"]
    start_date: str
    end_date: str
    retrieved_at: datetime
    bars: list[market_data.Bar]


@app.get("/healthz")
async def healthz() -> dict[str, str]:
    return {"service": "quant", "status": "ok", "version": "0.1.0"}


@app.post("/v1/market-data", response_model=MarketDataResponse)
async def fetch_market_data(request: MarketDataRequest) -> MarketDataResponse:
    try:
        start_date = date.fromisoformat(request.start_date)
        end_date = date.fromisoformat(request.end_date)
    except ValueError as error:
        raise HTTPException(status_code=422, detail="dates must use YYYY-MM-DD") from error
    if end_date < start_date:
        raise HTTPException(status_code=422, detail="end_date is before start_date")

    try:
        provider = market_data.get_provider(os.getenv("MARKET_DATA_PROVIDER", "yfinance").strip().lower())
    except market_data.UnknownProviderError as error:
        raise HTTPException(status_code=501, detail=f"unsupported market data provider {error}") from error

    try:
        records = provider.fetch_daily_bars(request.symbols, start_date, end_date)
    except market_data.MarketDataUnavailable as error:
        raise HTTPException(status_code=502, detail=str(error)) from error

    bars = market_data.normalize_bars(records)
    if not bars:
        raise HTTPException(status_code=422, detail="provider returned no usable bars")

    return MarketDataResponse(
        provider=provider.name,
        interval=request.interval,
        start_date=request.start_date,
        end_date=request.end_date,
        retrieved_at=datetime.now(timezone.utc),
        bars=bars,
    )


@app.post("/v1/backtests", response_model=BacktestResponse)
async def run_backtest(request: BacktestRequest) -> BacktestResponse:
    snapshot_root = Path(os.getenv("DOCUMENT_STORAGE_PATH", "/var/lib/signalledger/documents"))
    try:
        result = backtest.run_backtest(
            snapshot_root=snapshot_root,
            spec=request.spec,
            snapshot_storage_key=request.snapshot_storage_key,
            snapshot_checksum=request.snapshot_checksum,
            document_cutoff_at=request.document_cutoff_at,
            parameters=request.parameters,
            signals=[signal.model_dump() for signal in request.signals],
        )
    except backtest.BacktestError as error:
        raise HTTPException(status_code=422, detail=str(error)) from error
    return BacktestResponse.model_validate(result)


@app.post("/v1/extract-claims", response_model=ClaimExtractionResponse)
async def extract_claims(request: ClaimExtractionRequest) -> ClaimExtractionResponse:
    storage_root = Path(os.getenv("DOCUMENT_STORAGE_PATH", "/var/lib/signalledger/documents"))
    llm_provider = os.getenv("LLM_PROVIDER", "disabled").strip().lower()

    try:
        if llm_provider == "anthropic":
            pages = extract_pages(storage_root, request.storage_key)
            result = {
                "pages": pages,
                "claims": llm_extractor.extract_claims_with_llm(pages, request.effective_at),
                "extractor": llm_extractor.extractor_name(),
            }
        else:
            result = extract_pdf(storage_root, request.storage_key, request.effective_at)
    except FileNotFoundError as error:
        raise HTTPException(status_code=404, detail="document file was not found") from error
    except ExtractionError as error:
        raise HTTPException(status_code=422, detail=str(error)) from error
    except LLMExtractionError as error:
        raise HTTPException(status_code=502, detail=str(error)) from error
    return ClaimExtractionResponse.model_validate(result)
