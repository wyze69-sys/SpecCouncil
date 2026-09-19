package review

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
)

func testSnapshot(t *testing.T) evidence.Snapshot {
	t.Helper()
	snap, err := evidence.Freeze("snap-1", []evidence.Unit{
		{ID: "R-1", Kind: evidence.UnitRequirement, Text: "Users can edit their own projects."},
		{ID: "C-1", Kind: evidence.UnitComponent, Text: "The project service owns project writes."},
		{ID: "F-1", Kind: evidence.UnitFlow, Text: "The client calls the project service."},
	})
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	return snap
}

// mkFinding renders one finding object with the given id, severity and refs.
func mkFinding(id, severity string, refs ...string) string {
	quoted := make([]string, len(refs))
	for i, r := range refs {
		quoted[i] = strconv.Quote(r)
	}
	return fmt.Sprintf(
		`{"id":%q,"severity":%q,"category":"authorization","issue":"Ownership is unspecified.","recommendation":"State who may edit a project.","basis_refs":[%s]}`,
		id, severity, strings.Join(quoted, ","))
}

// mkResult renders a provider body containing the given findings.
func mkResult(findings ...string) string {
	return `{"findings":[` + strings.Join(findings, ",") + `]}`
}

func validBody() string { return mkResult(mkFinding("F-1", "high", "R-1")) }

func categoryOf(t *testing.T, err error) domain.ErrorCategory {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error %v is not a *ValidationError", err)
	}
	return ve.Category
}

func TestDecodeAndValidateAcceptsGoodOutput(t *testing.T) {
	snap := testSnapshot(t)

	got, err := DecodeAndValidate([]byte(validBody()), snap)
	if err != nil {
		t.Fatalf("DecodeAndValidate: %v", err)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(got.Findings))
	}
	if got.Findings[0].ID != "F-1" {
		t.Errorf("finding id = %q, want F-1", got.Findings[0].ID)
	}
}

func TestEmptyFindingsListIsAValidSuccess(t *testing.T) {
	if _, err := DecodeAndValidate([]byte(`{"findings":[]}`), testSnapshot(t)); err != nil {
		t.Fatalf("empty findings must be valid, got %v", err)
	}
}

func TestInvalidJSONIsRejected(t *testing.T) {
	snap := testSnapshot(t)
	cases := map[string]string{
		"empty body":       "",
		"not json":         "I think the design is fine.",
		"unknown field":    `{"findings":[],"summary":"extra"}`,
		"unknown in item":  mkResult(`{"id":"F-1","severity":"high","category":"c","issue":"i","recommendation":"r","basis_refs":["R-1"],"confidence":0.9}`),
		"trailing content": validBody() + `{"findings":[]}`,
	}
	for name, body := range cases {
		_, err := DecodeAndValidate([]byte(body), snap)
		if err == nil {
			t.Errorf("%s: accepted, want rejection", name)
			continue
		}
		if got := categoryOf(t, err); got != domain.ErrInvalidJSON {
			t.Errorf("%s: category = %s, want %s", name, got, domain.ErrInvalidJSON)
		}
	}
}

func TestSchemaBoundsAreEnforced(t *testing.T) {
	snap := testSnapshot(t)

	manyFindings := make([]string, 0, domain.MaxFindingsPerRole+1)
	for i := 0; i <= domain.MaxFindingsPerRole; i++ {
		manyFindings = append(manyFindings, mkFinding(fmt.Sprintf("F-%d", i), "low", "R-1"))
	}

	longIssue := mkResult(fmt.Sprintf(
		`{"id":"F-1","severity":"low","category":"c","issue":%s,"recommendation":"r","basis_refs":["R-1"]}`,
		strconv.Quote(strings.Repeat("x", domain.MaxIssueChars+1))))

	cases := map[string]string{
		"too many findings": mkResult(manyFindings...),
		"no basis refs":     mkResult(mkFinding("F-1", "low")),
		"too many refs": mkResult(mkFinding("F-1", "low",
			"R-1", "C-1", "F-1", "R-1", "C-1", "F-1")),
		"duplicate refs":   mkResult(mkFinding("F-1", "low", "R-1", "R-1")),
		"unknown severity": mkResult(mkFinding("F-1", "catastrophic", "R-1")),
		"empty issue":      mkResult(`{"id":"F-1","severity":"low","category":"c","issue":" ","recommendation":"r","basis_refs":["R-1"]}`),
		"issue too long":   longIssue,
		"duplicate ids":    mkResult(mkFinding("F-1", "low", "R-1"), mkFinding("F-1", "low", "C-1")),
	}
	for name, body := range cases {
		_, err := DecodeAndValidate([]byte(body), snap)
		if err == nil {
			t.Errorf("%s: accepted, want rejection", name)
			continue
		}
		if got := categoryOf(t, err); got != domain.ErrSchemaInvalid {
			t.Errorf("%s: category = %s, want %s", name, got, domain.ErrSchemaInvalid)
		}
	}
}

func TestBasisRefMustExistInTheSnapshot(t *testing.T) {
	snap := testSnapshot(t)

	_, err := DecodeAndValidate([]byte(mkResult(mkFinding("F-1", "high", "R-404"))), snap)
	if err == nil {
		t.Fatal("invented basis_ref accepted, want rejection")
	}
	if got := categoryOf(t, err); got != domain.ErrInvalidBasisRef {
		t.Errorf("category = %s, want %s", got, domain.ErrInvalidBasisRef)
	}
}
