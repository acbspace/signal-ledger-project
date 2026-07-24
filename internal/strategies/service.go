package strategies

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"signalledger/internal/domain"
)

var (
	ErrInvalidSpec  = errors.New("invalid strategy spec")
	ErrNoClaims     = errors.New("at least one claim is required")
	ErrEmptyDraft   = errors.New("no accepted claims matched the draft request")
	ErrClaimMissing = errors.New("one or more claims were not found")
)

type Repository interface {
	GetClaimsByIDs(context.Context, []string) ([]domain.StoredClaim, error)
	ListClaimsByDocument(context.Context, string) ([]domain.StoredClaim, error)
	CreateStrategyWithCitations(context.Context, domain.CreateStrategyInput) (domain.Strategy, error)
	ListStrategies(context.Context) ([]domain.Strategy, error)
	GetStrategyWithCitations(context.Context, string) (domain.Strategy, []domain.StoredClaim, error)
}

type Service struct {
	repository Repository
}

func NewService(repository Repository) *Service {
	return &Service{repository: repository}
}

type DraftInput struct {
	ClaimIDs    []string
	DocumentIDs []string
}

type CreateInput struct {
	Spec     Spec
	ClaimIDs []string
}

// Draft proposes a spec from accepted claims without persisting anything. The
// caller reviews or edits the proposal, then commits it through Create.
func (service *Service) Draft(ctx context.Context, input DraftInput) (Draft, error) {
	if len(input.ClaimIDs) == 0 && len(input.DocumentIDs) == 0 {
		return Draft{}, fmt.Errorf("%w: provide claim_ids or document_ids", ErrNoClaims)
	}

	claims := []domain.StoredClaim{}
	if len(input.ClaimIDs) > 0 {
		found, err := service.repository.GetClaimsByIDs(ctx, input.ClaimIDs)
		if err != nil {
			return Draft{}, err
		}
		if len(found) != len(input.ClaimIDs) {
			return Draft{}, ErrClaimMissing
		}
		claims = append(claims, found...)
	}
	for _, documentID := range input.DocumentIDs {
		found, err := service.repository.ListClaimsByDocument(ctx, documentID)
		if err != nil {
			return Draft{}, err
		}
		claims = append(claims, found...)
	}

	draft, err := BuildDraft(claims)
	if err != nil {
		return Draft{}, fmt.Errorf("%w: %v", ErrEmptyDraft, err)
	}
	return draft, nil
}

// Create validates the spec, verifies every cited claim is accepted, and stores
// the next immutable version for the slug together with its citations.
func (service *Service) Create(ctx context.Context, input CreateInput) (domain.Strategy, error) {
	if len(input.ClaimIDs) == 0 {
		return domain.Strategy{}, ErrNoClaims
	}

	spec := input.Spec
	// The server assigns the version during insert; normalize so a caller
	// submitting version 0 or a stale number still passes validation.
	spec.Version = 1
	if err := spec.Validate(); err != nil {
		return domain.Strategy{}, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}

	encoded, err := json.Marshal(spec)
	if err != nil {
		return domain.Strategy{}, fmt.Errorf("encode strategy spec: %w", err)
	}

	return service.repository.CreateStrategyWithCitations(ctx, domain.CreateStrategyInput{
		Slug:     spec.Slug,
		Name:     spec.Name,
		Spec:     encoded,
		ClaimIDs: input.ClaimIDs,
	})
}

func (service *Service) List(ctx context.Context) ([]domain.Strategy, error) {
	return service.repository.ListStrategies(ctx)
}

func (service *Service) Get(ctx context.Context, id string) (domain.Strategy, []domain.StoredClaim, error) {
	return service.repository.GetStrategyWithCitations(ctx, id)
}
