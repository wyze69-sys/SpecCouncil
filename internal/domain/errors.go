package domain

// ErrorCategory is the typed reason a role failed.
//
// A failed role produces zero findings; the category is the only explanation
// persisted for that role.
type ErrorCategory string

const (
	// ErrTransport covers network faults, retryable 5xx and 429 responses.
	ErrTransport ErrorCategory = "transport"
	// ErrTimeout covers a provider attempt that exceeded CALL_TIMEOUT.
	ErrTimeout ErrorCategory = "timeout"
	// ErrProviderRejected covers fatal 4xx, auth failures and provider refusals.
	// It is not retryable and never triggers a second call.
	ErrProviderRejected ErrorCategory = "provider_rejected"
	// ErrBudgetExhausted means the assembled prompt exceeded the model input
	// budget, so no provider call was made at all.
	ErrBudgetExhausted ErrorCategory = "budget_exhausted"
	// ErrInvalidJSON means the transport succeeded but the body was not strict JSON.
	ErrInvalidJSON ErrorCategory = "invalid_json"
	// ErrSchemaInvalid means the JSON parsed but violated the frozen output schema.
	ErrSchemaInvalid ErrorCategory = "schema_invalid"
	// ErrInvalidBasisRef means a basis_ref did not exist in the frozen snapshot.
	ErrInvalidBasisRef ErrorCategory = "invalid_basis_ref"
)

// IsRetryableTransport reports whether the category permits a transport retry.
func (c ErrorCategory) IsRetryableTransport() bool {
	return c == ErrTransport || c == ErrTimeout
}

// IsValidationCategory reports whether the category came from validating a
// transport-valid provider response.
func (c ErrorCategory) IsValidationCategory() bool {
	switch c {
	case ErrInvalidJSON, ErrSchemaInvalid, ErrInvalidBasisRef:
		return true
	}
	return false
}

// CallPurpose records why a provider call was made.
type CallPurpose string

const (
	// PurposeInitial is the first attempt of a role.
	PurposeInitial CallPurpose = "initial"
	// PurposeTransportRetry is the second attempt after a transport failure.
	PurposeTransportRetry CallPurpose = "transport_retry"
	// PurposeFormatRepair is the second attempt after a transport-valid but
	// invalid response.
	PurposeFormatRepair CallPurpose = "format_repair"
)

// MaxProviderCallsPerRole is the hard per-role provider-call budget.
// Call 2 is a transport retry XOR a format repair, never both.
const MaxProviderCallsPerRole = 2
