package domain

import "time"

// MarketDataRequest describes one reproducible market-data retrieval. Dates use
// the ISO calendar-date form (2006-01-02) and are inclusive on both ends.
type MarketDataRequest struct {
	Symbols   []string `json:"symbols"`
	StartDate string   `json:"start_date"`
	EndDate   string   `json:"end_date"`
	Interval  string   `json:"interval"`
}

// Bar is one normalized daily observation for a symbol.
type Bar struct {
	Symbol   string  `json:"symbol"`
	Date     string  `json:"date"`
	Open     float64 `json:"open"`
	High     float64 `json:"high"`
	Low      float64 `json:"low"`
	Close    float64 `json:"close"`
	AdjClose float64 `json:"adj_close"`
	Volume   int64   `json:"volume"`
}

// MarketDataResult is the quant service's normalized answer to one request.
type MarketDataResult struct {
	Provider    string
	Bars        []Bar
	RetrievedAt time.Time
}

// MarketDataSnapshot records provider, parameters, retrieval time, storage key,
// and checksum so a later backtest can prove exactly which data it consumed.
type MarketDataSnapshot struct {
	ID          string
	Provider    string
	Symbols     []string
	Interval    string
	StartDate   string
	EndDate     string
	StorageKey  *string
	Checksum    *string
	RetrievedAt time.Time
}

type CreateMarketDataSnapshotInput struct {
	Provider    string
	Symbols     []string
	Interval    string
	StartDate   string
	EndDate     string
	StorageKey  string
	Checksum    string
	RetrievedAt time.Time
}
