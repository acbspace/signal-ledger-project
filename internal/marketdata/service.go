package marketdata

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"signalledger/internal/domain"
)

var ErrInvalidRequest = errors.New("invalid market data request")

var symbolPattern = regexp.MustCompile(`^[A-Z.]{1,10}$`)

type Repository interface {
	CreateMarketDataJob(context.Context, domain.MarketDataRequest) (domain.Job, error)
	InsertMarketDataSnapshot(context.Context, domain.CreateMarketDataSnapshotInput) (domain.MarketDataSnapshot, error)
	GetMarketDataSnapshot(context.Context, string) (domain.MarketDataSnapshot, error)
	ListMarketDataSnapshots(context.Context) ([]domain.MarketDataSnapshot, error)
}

type Fetcher interface {
	FetchMarketData(context.Context, domain.MarketDataRequest) (domain.MarketDataResult, error)
}

type Service struct {
	repository Repository
	fetcher    Fetcher
	files      SnapshotStore
}

func NewService(repository Repository, fetcher Fetcher, files SnapshotStore) *Service {
	return &Service{repository: repository, fetcher: fetcher, files: files}
}

// RequestSnapshot validates the request and enqueues a durable job; the worker
// performs the retrieval so provider flakiness gets the queue's retry policy.
func (service *Service) RequestSnapshot(ctx context.Context, request domain.MarketDataRequest) (domain.Job, error) {
	if err := validateRequest(request); err != nil {
		return domain.Job{}, err
	}
	return service.repository.CreateMarketDataJob(ctx, request)
}

// FetchSnapshot is the worker-side path: call the quant service, persist the
// canonical CSV to the shared volume, and record the snapshot row.
func (service *Service) FetchSnapshot(ctx context.Context, request domain.MarketDataRequest) (domain.MarketDataSnapshot, error) {
	if err := validateRequest(request); err != nil {
		return domain.MarketDataSnapshot{}, err
	}

	result, err := service.fetcher.FetchMarketData(ctx, request)
	if err != nil {
		return domain.MarketDataSnapshot{}, err
	}
	if len(result.Bars) == 0 {
		return domain.MarketDataSnapshot{}, fmt.Errorf("provider returned no bars")
	}

	csv := CanonicalCSV(result.Bars)
	storageKey, err := service.files.Save(csv)
	if err != nil {
		return domain.MarketDataSnapshot{}, err
	}

	return service.repository.InsertMarketDataSnapshot(ctx, domain.CreateMarketDataSnapshotInput{
		Provider:    result.Provider,
		Symbols:     request.Symbols,
		Interval:    request.Interval,
		StartDate:   request.StartDate,
		EndDate:     request.EndDate,
		StorageKey:  storageKey,
		Checksum:    Checksum(csv),
		RetrievedAt: result.RetrievedAt,
	})
}

func (service *Service) Get(ctx context.Context, id string) (domain.MarketDataSnapshot, error) {
	return service.repository.GetMarketDataSnapshot(ctx, id)
}

func (service *Service) List(ctx context.Context) ([]domain.MarketDataSnapshot, error) {
	return service.repository.ListMarketDataSnapshots(ctx)
}

func validateRequest(request domain.MarketDataRequest) error {
	if len(request.Symbols) == 0 {
		return fmt.Errorf("%w: at least one symbol is required", ErrInvalidRequest)
	}
	for _, symbol := range request.Symbols {
		if !symbolPattern.MatchString(symbol) {
			return fmt.Errorf("%w: invalid symbol %q", ErrInvalidRequest, symbol)
		}
	}
	if request.Interval != "1d" {
		return fmt.Errorf("%w: only the 1d interval is supported", ErrInvalidRequest)
	}
	start, err := time.Parse("2006-01-02", request.StartDate)
	if err != nil {
		return fmt.Errorf("%w: start_date must use YYYY-MM-DD", ErrInvalidRequest)
	}
	end, err := time.Parse("2006-01-02", request.EndDate)
	if err != nil {
		return fmt.Errorf("%w: end_date must use YYYY-MM-DD", ErrInvalidRequest)
	}
	if end.Before(start) {
		return fmt.Errorf("%w: end_date is before start_date", ErrInvalidRequest)
	}
	return nil
}
