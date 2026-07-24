import json
from dataclasses import dataclass
from datetime import datetime, timezone

from quant_service import llm_extractor


@dataclass
class FakeBlock:
    type: str
    text: str


class FakeResponse:
    def __init__(self, payload: dict) -> None:
        self.stop_reason = "end_turn"
        self.content = [FakeBlock(type="text", text=json.dumps(payload))]


class FakeMessages:
    def __init__(self, payload: dict) -> None:
        self.payload = payload
        self.requests: list[dict] = []

    def create(self, **kwargs):
        self.requests.append(kwargs)
        return FakeResponse(self.payload)


class FakeClient:
    def __init__(self, payload: dict) -> None:
        self.messages = FakeMessages(payload)


PAGES = [
    {
        "page_number": 1,
        "content": "Oil demand destruction appears smaller than feared. Contact us for details.",
    }
]
EFFECTIVE_AT = datetime(2026, 1, 1, tzinfo=timezone.utc)


def claim_payload(quote: str, **overrides) -> dict:
    claim = {
        "page_number": 1,
        "ticker": None,
        "claim": "Oil demand is holding up better than consensus feared.",
        "evidence_quote": quote,
        "claim_kind": "macro",
        "direction": "positive",
        "horizon_days": 90,
        "confidence": 0.7,
    }
    claim.update(overrides)
    return claim


def test_verbatim_evidence_is_kept() -> None:
    client = FakeClient({"claims": [claim_payload("Oil demand destruction appears smaller than feared.")]})

    claims = llm_extractor.extract_claims_with_llm(PAGES, EFFECTIVE_AT, client=client)

    assert len(claims) == 1
    assert claims[0]["claim_kind"] == "macro"
    assert claims[0]["effective_at"] == EFFECTIVE_AT


def test_fabricated_evidence_is_dropped() -> None:
    client = FakeClient({"claims": [claim_payload("Oil prices will certainly double next year.")]})

    claims = llm_extractor.extract_claims_with_llm(PAGES, EFFECTIVE_AT, client=client)

    assert claims == []


def test_invalid_ticker_and_horizon_are_nulled() -> None:
    client = FakeClient(
        {
            "claims": [
                claim_payload(
                    "Oil demand destruction appears smaller than feared.",
                    ticker="not a ticker",
                    horizon_days=-5,
                )
            ]
        }
    )

    claims = llm_extractor.extract_claims_with_llm(PAGES, EFFECTIVE_AT, client=client)

    assert claims[0]["ticker"] is None
    assert claims[0]["horizon_days"] is None


def test_request_uses_structured_output_schema() -> None:
    client = FakeClient({"claims": []})

    llm_extractor.extract_claims_with_llm(PAGES, EFFECTIVE_AT, client=client)

    request = client.messages.requests[0]
    assert request["output_config"]["format"]["type"] == "json_schema"
    assert request["thinking"] == {"type": "adaptive"}
