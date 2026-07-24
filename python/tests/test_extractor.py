"""The prose gate that keeps page furniture out of the claim stream.

These are unit tests for `looks_like_prose`, plus one end-to-end check that a
page mixing real analysis with captions and a byline yields only the analysis.
"""

from datetime import datetime, timezone

import fitz
import pytest

from quant_service.extractor import extract_pdf, looks_like_prose

REAL_CLAIMS = [
    "Revenue growth is expected to accelerate this year as demand broadens.",
    "We think oil prices face downside risk into year-end on weaker demand.",
    "Nominal GDP growth is likely to be in the mid-single digits next year.",
    "Credit spreads should stay contained given the more mature stage of the cycle.",
]

FURNITURE = [
    # Exhibit / figure / table / source / note captions.
    "Exhibit 2: Earnings transcripts flag rising material costs and supply chain risk",
    "Figure 4: US real yields versus the dollar, indexed to 100",
    "Table 1: Key forecasts for growth and inflation across regions",
    "Source: Goldman Sachs Global Investment Research Exhibit 2: Our Approach",
    "Note: The Global Supply Chain Pressure Index is normalized to a zero mean.",
    # A byline with contact details.
    "Filippo Cuscito +44(20)7051-9073 | filippo.cuscito@gs.com Goldman Sachs International",
    # A chart axis: almost all digits and separators.
    "-1.5 1.5 4.8 6.6 7.0 9.1 -5 0 5 10 15 20 25 30 South Africa growth",
    # Legal boilerplate.
    "Trading ideas and investment strategies discussed herein may give rise to risk.",
    "These instruments are not suitable for all investors and may lose value.",
]


@pytest.mark.parametrize("sentence", REAL_CLAIMS)
def test_real_prose_passes(sentence: str) -> None:
    assert looks_like_prose(sentence)


@pytest.mark.parametrize("sentence", FURNITURE)
def test_furniture_is_rejected(sentence: str) -> None:
    assert not looks_like_prose(sentence)


def test_gate_is_case_insensitive_and_deterministic() -> None:
    caption = "EXHIBIT 9: global debt reached a record high this year"
    assert looks_like_prose(caption) is False
    assert looks_like_prose(caption) is looks_like_prose(caption)


def test_extract_pdf_drops_furniture_but_keeps_analysis(tmp_path) -> None:
    document_path = tmp_path / "documents" / "report.pdf"
    document_path.parent.mkdir()
    document = fitz.open()
    page = document.new_page()
    page.insert_text(
        (72, 72),
        # Two captions and a byline around one genuine claim. Only the claim has a
        # direction term, but the gate must also spare it while dropping the rest.
        "Exhibit 3: Oil demand by region. "
        "Global oil demand growth should accelerate into year-end on resilient consumption. "
        "Source: Goldman Sachs Global Investment Research. "
        "Jane Analyst +44(20)1234-5678 | jane.analyst@example.com",
    )
    document.save(document_path)
    document.close()

    result = extract_pdf(tmp_path, "documents/report.pdf", datetime(2026, 1, 1, tzinfo=timezone.utc))

    assert result["extractor"] == "pymupdf-heuristic-v2"
    texts = [claim["claim"] for claim in result["claims"]]
    assert any("demand growth should accelerate" in text for text in texts)
    assert not any(text.startswith(("Exhibit", "Source")) for text in texts)
    assert not any("@example.com" in text for text in texts)
