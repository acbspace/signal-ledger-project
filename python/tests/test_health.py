from datetime import datetime, timezone

import fitz
from fastapi.testclient import TestClient

from quant_service.extractor import extract_pdf
from quant_service.main import app


def test_healthz() -> None:
    response = TestClient(app).get("/healthz")

    assert response.status_code == 200
    assert response.json()["service"] == "quant"


def test_extract_pdf_preserves_page_citations(tmp_path) -> None:
    document_path = tmp_path / "documents" / "report.pdf"
    document_path.parent.mkdir()
    document = fitz.open()
    page = document.new_page()
    page.insert_text(
        (72, 72),
        "Revenue growth is expected to accelerate this year. Margin risk remains elevated.",
    )
    document.save(document_path)
    document.close()

    result = extract_pdf(
        tmp_path,
        "documents/report.pdf",
        datetime(2025, 1, 1, tzinfo=timezone.utc),
    )

    assert result["pages"][0]["page_number"] == 1
    assert result["claims"]
    assert all(claim["page_number"] == 1 for claim in result["claims"])


def test_extract_claims_endpoint(tmp_path, monkeypatch) -> None:
    document_path = tmp_path / "documents" / "report.pdf"
    document_path.parent.mkdir()
    document = fitz.open()
    page = document.new_page()
    page.insert_text((72, 72), "Demand growth should improve next quarter.")
    document.save(document_path)
    document.close()
    monkeypatch.setenv("DOCUMENT_STORAGE_PATH", str(tmp_path))

    response = TestClient(app).post(
        "/v1/extract-claims",
        json={
            "document_id": "f1c57343-769c-4f85-9f27-53790c7c4e8a",
            "storage_key": "documents/report.pdf",
            "effective_at": "2025-01-01T00:00:00Z",
        },
    )

    assert response.status_code == 200
    assert response.json()["pages"][0]["page_number"] == 1
