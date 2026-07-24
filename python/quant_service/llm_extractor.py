"""Anthropic-backed claim extraction with a verbatim-evidence gate.

Claims are only kept when their evidence quote appears (whitespace-normalized)
in the cited page's extracted text. That gate is the load-bearing invariant of
the pipeline: a claim that cannot point at its own source sentence is dropped
rather than stored, so hallucinated evidence never reaches the database.
"""

from __future__ import annotations

import json
import os
import re
from datetime import datetime
from typing import Any

import anthropic


class LLMExtractionError(RuntimeError):
    """Provider-side failure. Surfaced as a 502 so the Go job queue retries."""


_DEFAULT_MODEL = "claude-opus-4-8"
_TICKER_PATTERN = re.compile(r"^[A-Z.]{1,10}$")
_CLAIM_KINDS = {"fundamental", "macro", "risk", "catalyst", "valuation"}
_DIRECTIONS = {"positive", "negative", "neutral"}
_MAX_CLAIMS = 100
_PAGES_PER_REQUEST = 6

_SYSTEM_PROMPT = """You extract investment-relevant claims from sell-side research pages.

A claim is a falsifiable statement about markets, macro variables, sectors, or
specific securities that could inform a trading strategy. Skip disclaimers,
boilerplate, tables of contents, and analyst contact details.

For every claim you must copy one supporting sentence verbatim from the page
into evidence_quote — character-for-character, no paraphrasing, no ellipses.
Set ticker only when the claim is about one listed company (bare symbol, e.g.
XOM); use null for macro, sector, or multi-name claims. horizon_days is the
approximate forward horizon of the view in days, or null when unstated.
confidence is your read of the author's conviction between 0 and 1."""

_CLAIM_SCHEMA: dict[str, Any] = {
    "type": "object",
    "additionalProperties": False,
    "required": ["claims"],
    "properties": {
        "claims": {
            "type": "array",
            "items": {
                "type": "object",
                "additionalProperties": False,
                "required": [
                    "page_number",
                    "ticker",
                    "claim",
                    "evidence_quote",
                    "claim_kind",
                    "direction",
                    "horizon_days",
                    "confidence",
                ],
                "properties": {
                    "page_number": {"type": "integer"},
                    "ticker": {"type": ["string", "null"]},
                    "claim": {"type": "string"},
                    "evidence_quote": {"type": "string"},
                    "claim_kind": {
                        "type": "string",
                        "enum": sorted(_CLAIM_KINDS),
                    },
                    "direction": {
                        "type": "string",
                        "enum": sorted(_DIRECTIONS),
                    },
                    "horizon_days": {"type": ["integer", "null"]},
                    "confidence": {"type": "number"},
                },
            },
        }
    },
}


def model_name() -> str:
    return os.getenv("LLM_MODEL", _DEFAULT_MODEL)


def extractor_name() -> str:
    return f"anthropic:{model_name()}-v1"


def extract_claims_with_llm(
    pages: list[dict[str, Any]],
    effective_at: datetime,
    client: anthropic.Anthropic | None = None,
) -> list[dict[str, Any]]:
    """Extract verified claims from extracted pages via the Anthropic API."""

    if client is None:
        client = anthropic.Anthropic(api_key=os.getenv("LLM_API_KEY") or None)

    claims: list[dict[str, Any]] = []
    for batch in _batches(pages, _PAGES_PER_REQUEST):
        if len(claims) >= _MAX_CLAIMS:
            break
        raw = _request_batch(client, batch)
        claims.extend(_verified_claims(raw, batch, effective_at))
    return claims[:_MAX_CLAIMS]


def _batches(pages: list[dict[str, Any]], size: int) -> list[list[dict[str, Any]]]:
    return [pages[index : index + size] for index in range(0, len(pages), size)]


def _request_batch(client: anthropic.Anthropic, batch: list[dict[str, Any]]) -> list[dict[str, Any]]:
    prompt = "\n\n".join(
        f"<page number=\"{page['page_number']}\">\n{page['content']}\n</page>"
        for page in batch
    )

    try:
        response = client.messages.create(
            model=model_name(),
            max_tokens=16000,
            thinking={"type": "adaptive"},
            output_config={"format": {"type": "json_schema", "schema": _CLAIM_SCHEMA}},
            system=_SYSTEM_PROMPT,
            messages=[{"role": "user", "content": prompt}],
        )
    except anthropic.RateLimitError as error:
        raise LLMExtractionError("anthropic rate limit") from error
    except anthropic.APIStatusError as error:
        raise LLMExtractionError(f"anthropic API error {error.status_code}") from error
    except anthropic.APIConnectionError as error:
        raise LLMExtractionError("anthropic connection failure") from error

    if response.stop_reason == "refusal":
        return []
    if response.stop_reason == "max_tokens":
        raise LLMExtractionError("anthropic response was truncated")

    text = "".join(block.text for block in response.content if block.type == "text")
    try:
        parsed = json.loads(text)
    except json.JSONDecodeError as error:
        raise LLMExtractionError("anthropic response was not valid JSON") from error
    claims = parsed.get("claims") if isinstance(parsed, dict) else None
    if not isinstance(claims, list):
        raise LLMExtractionError("anthropic response missed the claims schema")
    return claims


def _verified_claims(
    raw_claims: list[dict[str, Any]],
    batch: list[dict[str, Any]],
    effective_at: datetime,
) -> list[dict[str, Any]]:
    normalized_pages = {
        page["page_number"]: _normalize(page["content"]) for page in batch
    }

    verified: list[dict[str, Any]] = []
    for raw in raw_claims:
        if not isinstance(raw, dict):
            continue
        page_number = raw.get("page_number")
        quote = raw.get("evidence_quote")
        claim_text = raw.get("claim")
        if page_number not in normalized_pages:
            continue
        if not isinstance(quote, str) or not isinstance(claim_text, str):
            continue
        if not claim_text.strip():
            continue
        # The gate: the quote must exist verbatim (modulo whitespace) on the page.
        if _normalize(quote) not in normalized_pages[page_number]:
            continue
        if raw.get("claim_kind") not in _CLAIM_KINDS or raw.get("direction") not in _DIRECTIONS:
            continue

        confidence = raw.get("confidence")
        if not isinstance(confidence, (int, float)):
            continue
        confidence = min(max(float(confidence), 0.0), 1.0)

        ticker = raw.get("ticker")
        if not isinstance(ticker, str) or not _TICKER_PATTERN.match(ticker):
            ticker = None

        horizon_days = raw.get("horizon_days")
        if not isinstance(horizon_days, int) or horizon_days < 1:
            horizon_days = None

        verified.append(
            {
                "page_number": page_number,
                "ticker": ticker,
                "claim": " ".join(claim_text.split())[:2000],
                "evidence_quote": " ".join(quote.split())[:3000],
                "claim_kind": raw["claim_kind"],
                "direction": raw["direction"],
                "horizon_days": horizon_days,
                "confidence": confidence,
                "effective_at": effective_at,
            }
        )
    verified.sort(key=lambda claim: claim["page_number"])
    return verified


def _normalize(text: str) -> str:
    return " ".join(text.split())
