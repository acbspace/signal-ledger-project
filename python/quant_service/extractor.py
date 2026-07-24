"""Deterministic PDF text extraction and baseline source-cited claims.

The heuristic extractor is intentionally modest. It proves the document-to-claim
pipeline without disguising a keyword matcher as investment intelligence. A later
LLM adapter can replace `extract_claims_from_text` while preserving this response
contract and its page-level evidence requirement.

`looks_like_prose` gates the candidate sentences first: research PDFs interleave
analysis with page furniture — bylines, exhibit captions, chart axes, legal
boilerplate — and surfacing that as claims poisons every downstream layer that
treats a claim as evidence. The gate judges the shape of the text, not the
analysis, so a claim that clears it can still be wrong.
"""

from __future__ import annotations

import re
from datetime import datetime
from pathlib import Path
from typing import Any

import fitz


class ExtractionError(ValueError):
    """The document cannot be safely or meaningfully extracted."""


_SENTENCE_BOUNDARY = re.compile(r"(?<=[.!?])\s+")

# Prose gate. Research PDFs interleave analysis with page furniture — author
# bylines, exhibit captions, chart axes, legal boilerplate — and the extractor
# used to surface all of it as "claims". These patterns reject text that is not
# analytical prose. This is quality gating on the shape of the text, not an
# attempt to judge the analysis; a claim that clears the gate can still be wrong.
_EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")
# International contact numbers begin with a country code; prose rarely does.
_CONTACT_PHONE = re.compile(r"\+\d[\d\s().\-]{6,}\d")
_FURNITURE_PREFIX = re.compile(
    r"^(exhibit|figure|fig\.|table|chart|source|sources|note|notes"
    r"|appendix|footnote|disclosure|disclosures)\b",
    re.IGNORECASE,
)
# Specific enough to be legal boilerplate rather than analysis.
_DISCLAIMER_MARKERS = (
    "discussed herein",
    "not suitable for all investors",
    "constitutes investment advice",
    "past performance is",
)
_MIN_PROSE_WORDS = 6
_MIN_LETTER_RATIO = 0.65
_POSITIVE_TERMS = (
    "accelerat",
    "beat",
    "expand",
    "growth",
    "improve",
    "increase",
    "outperform",
    "strong",
    "upside",
)
_NEGATIVE_TERMS = (
    "declin",
    "decrease",
    "downside",
    "headwind",
    "pressure",
    "risk",
    "slow",
    "weak",
)
_MACRO_TERMS = ("inflation", "interest rate", "recession", "currency", "gdp", "tariff")
_CATALYST_TERMS = ("catalyst", "launch", "approval", "earnings", "guidance", "release")
_VALUATION_TERMS = ("valuation", "multiple", "price target", "discounted cash flow", "p/e")
_MAX_CLAIMS = 100


def extract_pages(storage_root: Path, storage_key: str) -> list[dict[str, Any]]:
    """Extract page-numbered text from a storage-key-scoped PDF."""

    path = resolve_storage_key(storage_root, storage_key)
    if not path.is_file():
        raise FileNotFoundError(storage_key)

    try:
        document = fitz.open(path)
    except (fitz.FileDataError, RuntimeError) as error:
        raise ExtractionError("document is not a readable PDF") from error

    pages: list[dict[str, Any]] = []
    try:
        for page_number, page in enumerate(document, start=1):
            content = page.get_text("text").strip()
            pages.append({"page_number": page_number, "content": content})
    finally:
        document.close()

    if not pages:
        raise ExtractionError("document contains no pages")
    return pages


def extract_pdf(storage_root: Path, storage_key: str, effective_at: datetime) -> dict[str, Any]:
    """Extract pages and baseline heuristic claims from a PDF."""

    pages = extract_pages(storage_root, storage_key)

    claims: list[dict[str, Any]] = []
    for page in pages:
        if len(claims) >= _MAX_CLAIMS:
            break
        claims.extend(
            extract_claims_from_text(
                page_number=page["page_number"],
                content=page["content"],
                effective_at=effective_at,
                remaining=_MAX_CLAIMS - len(claims),
            )
        )

    return {
        "pages": pages,
        "claims": claims,
        "extractor": "pymupdf-heuristic-v2",
    }


def resolve_storage_key(storage_root: Path, storage_key: str) -> Path:
    """Resolve only paths below the mounted document root."""

    root = storage_root.resolve()
    candidate = (root / storage_key).resolve()
    if candidate != root and root not in candidate.parents:
        raise ExtractionError("storage key escapes document root")
    return candidate


def extract_claims_from_text(
    *, page_number: int, content: str, effective_at: datetime, remaining: int
) -> list[dict[str, Any]]:
    claims: list[dict[str, Any]] = []
    for sentence in _SENTENCE_BOUNDARY.split(content):
        normalized = " ".join(sentence.split())
        if not 30 <= len(normalized) <= 1_500:
            continue
        if not looks_like_prose(normalized):
            continue

        lower = normalized.lower()
        positive_hits = sum(term in lower for term in _POSITIVE_TERMS)
        negative_hits = sum(term in lower for term in _NEGATIVE_TERMS)
        if positive_hits == 0 and negative_hits == 0:
            continue

        direction = "positive" if positive_hits > negative_hits else "negative"
        if positive_hits == negative_hits:
            direction = "neutral"

        claims.append(
            {
                "page_number": page_number,
                "ticker": None,
                "claim": normalized,
                "evidence_quote": normalized,
                "claim_kind": classify_claim(lower),
                "direction": direction,
                "horizon_days": None,
                "confidence": min(0.75, 0.5 + 0.05 * (positive_hits + negative_hits)),
                "effective_at": effective_at,
            }
        )
        if len(claims) >= remaining:
            break
    return claims


def looks_like_prose(sentence: str) -> bool:
    """Reject page furniture: bylines, captions, chart axes, boilerplate.

    A real analytical sentence is mostly letters and several words long, does not
    lead with a caption keyword, and carries no contact details or disclaimer
    language. Deterministic and case-insensitive so extraction stays reproducible.
    """

    if _EMAIL.search(sentence) or _CONTACT_PHONE.search(sentence):
        return False
    if _FURNITURE_PREFIX.match(sentence):
        return False

    lowered = sentence.lower()
    if any(marker in lowered for marker in _DISCLAIMER_MARKERS):
        return False

    non_space = [character for character in sentence if not character.isspace()]
    if not non_space:
        return False
    letters = sum(character.isalpha() for character in non_space)
    if letters / len(non_space) < _MIN_LETTER_RATIO:
        return False

    # A "word" is a token that is alphabetic apart from at most one trailing mark
    # (comma, period, apostrophe), so chart labels and number runs do not count.
    words = sum(
        1
        for token in sentence.split()
        if len(token) >= 2 and sum(character.isalpha() for character in token) >= len(token) - 1
    )
    return words >= _MIN_PROSE_WORDS


def classify_claim(lower_sentence: str) -> str:
    if any(term in lower_sentence for term in _VALUATION_TERMS):
        return "valuation"
    if any(term in lower_sentence for term in _MACRO_TERMS):
        return "macro"
    if any(term in lower_sentence for term in _CATALYST_TERMS):
        return "catalyst"
    if "risk" in lower_sentence or "headwind" in lower_sentence:
        return "risk"
    return "fundamental"
