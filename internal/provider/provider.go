package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

// Request is one provider attempt.
type Request struct {
	Role    domain.Role
	Purpose domain.CallPurpose
	Prompt  string
	// Attempt is 1 for the first call and 2 for the second.
	Attempt int
}

// Response is one raw provider attempt that reached the model and returned.
//
// Body is intentionally unstructured: parsing and validation are separate,
// testable steps and raw output never becomes trusted state.
type Response struct {
	Body      []byte
	Model     string
	TokensIn  int
	TokensOut int
}

// Error is a typed provider failure carrying its domain error category.
type Error struct {
	Category domain.ErrorCategory
	Message  string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Category)
	}
	return fmt.Sprintf("%s: %s", e.Category, e.Message)
}

// NewError builds a typed provider error.
func NewError(category domain.ErrorCategory, format string, args ...any) *Error {
	return &Error{Category: category, Message: fmt.Sprintf(format, args...)}
}

// CategoryOf extracts the domain category from a provider error.
//
// Any error that is not explicitly typed is treated as a transport fault,
// because an unclassified network failure is the safe default for retry logic.
func CategoryOf(err error) domain.ErrorCategory {
	var perr *Error
	if errors.As(err, &perr) {
		return perr.Category
	}
	return domain.ErrTransport
}

// Provider is the single adapter boundary of v1.
//
// v1 has exactly one provider and one model per review, frozen per review.
// There is no vendor fallback.
type Provider interface {
	Call(ctx context.Context, req Request) (Response, error)
}
