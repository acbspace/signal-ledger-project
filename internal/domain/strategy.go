package domain

import (
	"encoding/json"
	"errors"
	"time"
)

// ErrClaimNotCitable is returned when a strategy cites a claim that does not
// exist or has not been reviewed and accepted.
var ErrClaimNotCitable = errors.New("cited claim is missing or not accepted")

// Strategy is an immutable, versioned strategy specification. New submissions
// under an existing slug create the next version instead of mutating history.
type Strategy struct {
	ID        string
	Slug      string
	Version   int
	Name      string
	Spec      json.RawMessage
	CreatedAt time.Time
}

type CreateStrategyInput struct {
	Slug     string
	Name     string
	Spec     json.RawMessage
	ClaimIDs []string
}
