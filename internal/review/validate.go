package review

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
)

// ValidationError is a typed rejection of one provider response.
type ValidationError struct {
	Category domain.ErrorCategory
	Message  string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Category, e.Message)
}

// DecodeAndValidate turns a raw provider body into a trusted ReviewerResult.
//
// The pipeline is deliberately staged so each failure keeps its own category:
//
//	strict JSON parse            -> invalid_json
//	structural schema validation -> schema_invalid
//	semantic validation          -> invalid_basis_ref
//
// Raw provider output never becomes trusted state on its own.
func DecodeAndValidate(body []byte, snap evidence.Snapshot) (domain.ReviewerResult, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return domain.ReviewerResult{}, &ValidationError{
			Category: domain.ErrInvalidJSON,
			Message:  "empty provider body",
		}
	}

	var result domain.ReviewerResult
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		return domain.ReviewerResult{}, &ValidationError{
			Category: domain.ErrInvalidJSON,
			Message:  err.Error(),
		}
	}
	if dec.More() {
		return domain.ReviewerResult{}, &ValidationError{
			Category: domain.ErrInvalidJSON,
			Message:  "trailing content after the JSON object",
		}
	}

	if err := validateStructure(result); err != nil {
		return domain.ReviewerResult{}, err
	}
	if err := validateSemantics(result, snap); err != nil {
		return domain.ReviewerResult{}, err
	}
	return result, nil
}

// validateStructure enforces the frozen output schema.
func validateStructure(result domain.ReviewerResult) error {
	if len(result.Findings) > domain.MaxFindingsPerRole {
		return &ValidationError{
			Category: domain.ErrSchemaInvalid,
			Message:  fmt.Sprintf("response has %d findings, limit is %d", len(result.Findings), domain.MaxFindingsPerRole),
		}
	}

	seenIDs := make(map[string]struct{}, len(result.Findings))
	for i, f := range result.Findings {
		where := fmt.Sprintf("findings[%d]", i)

		if strings.TrimSpace(f.ID) == "" {
			return &ValidationError{Category: domain.ErrSchemaInvalid, Message: where + ": empty id"}
		}
		if _, dup := seenIDs[f.ID]; dup {
			return &ValidationError{Category: domain.ErrSchemaInvalid, Message: where + ": duplicate finding id " + f.ID}
		}
		seenIDs[f.ID] = struct{}{}

		if !domain.IsValidSeverity(f.Severity) {
			return &ValidationError{
				Category: domain.ErrSchemaInvalid,
				Message:  fmt.Sprintf("%s: unknown severity %q", where, f.Severity),
			}
		}
		if strings.TrimSpace(f.Category) == "" {
			return &ValidationError{Category: domain.ErrSchemaInvalid, Message: where + ": empty category"}
		}
		if strings.TrimSpace(f.Issue) == "" {
			return &ValidationError{Category: domain.ErrSchemaInvalid, Message: where + ": empty issue"}
		}
		if len(f.Issue) > domain.MaxIssueChars {
			return &ValidationError{
				Category: domain.ErrSchemaInvalid,
				Message:  fmt.Sprintf("%s: issue is %d characters, limit is %d", where, len(f.Issue), domain.MaxIssueChars),
			}
		}
		if strings.TrimSpace(f.Recommendation) == "" {
			return &ValidationError{Category: domain.ErrSchemaInvalid, Message: where + ": empty recommendation"}
		}
		if len(f.Recommendation) > domain.MaxRecommendationChars {
			return &ValidationError{
				Category: domain.ErrSchemaInvalid,
				Message:  fmt.Sprintf("%s: recommendation is %d characters, limit is %d", where, len(f.Recommendation), domain.MaxRecommendationChars),
			}
		}

		if strings.TrimSpace(string(f.Kind)) == "" {
			return &ValidationError{Category: domain.ErrSchemaInvalid, Message: where + ": empty kind"}
		}
		if !domain.IsValidFindingKind(f.Kind) {
			return &ValidationError{
				Category: domain.ErrSchemaInvalid,
				Message:  fmt.Sprintf("%s: unknown kind %q", where, f.Kind),
			}
		}
		minRefs, maxRefs, _ := domain.BasisRefsBounds(f.Kind)
		n := len(f.BasisRefs)
		if n < minRefs || n > maxRefs {
			return &ValidationError{
				Category: domain.ErrSchemaInvalid,
				Message:  fmt.Sprintf("%s: kind %q allows %d..%d basis_refs, got %d", where, f.Kind, minRefs, maxRefs, n),
			}
		}
		if (f.Kind == domain.FindingExisting || f.Kind == domain.FindingConflicting) && strings.TrimSpace(f.AnchorRef) != "" {
			return &ValidationError{
				Category: domain.ErrSchemaInvalid,
				Message:  where + ": anchor_ref is only allowed for a missing finding",
			}
		}
		if f.Kind == domain.FindingMissing && strings.TrimSpace(f.AnchorRef) == "" {
			return &ValidationError{
				Category: domain.ErrSchemaInvalid,
				Message:  where + ": missing finding requires exactly one anchor_ref",
			}
		}

		seenRefs := make(map[string]struct{}, n)
		for _, ref := range f.BasisRefs {
			if strings.TrimSpace(ref) == "" {
				return &ValidationError{Category: domain.ErrSchemaInvalid, Message: where + ": empty basis_ref"}
			}
			if _, dup := seenRefs[ref]; dup {
				return &ValidationError{Category: domain.ErrSchemaInvalid, Message: where + ": duplicate basis_ref " + ref}
			}
			seenRefs[ref] = struct{}{}
		}
	}
	return nil
}

// validateSemantics enforces that every citation resolves inside the frozen
// snapshot. A basis_ref that does not exist is never guessed or repaired.
func validateSemantics(result domain.ReviewerResult, snap evidence.Snapshot) error {
	for i, f := range result.Findings {
		for _, ref := range f.BasisRefs {
			if !snap.HasRef(ref) {
				return &ValidationError{
					Category: domain.ErrInvalidBasisRef,
					Message:  fmt.Sprintf("findings[%d] cites %q, which is not in snapshot %s", i, ref, snap.ID),
				}
			}
		}
		if f.AnchorRef != "" && !snap.HasRef(f.AnchorRef) {
			return &ValidationError{
				Category: domain.ErrInvalidBasisRef,
				Message:  fmt.Sprintf("findings[%d] anchors %q, which is not in snapshot %s", i, f.AnchorRef, snap.ID),
			}
		}
	}
	return nil
}
