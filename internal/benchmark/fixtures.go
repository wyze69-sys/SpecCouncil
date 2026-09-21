package benchmark

import (
	"context"
	"fmt"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/provider"
	"github.com/wyze69-sys/SpecCouncil/internal/provider/fake"
)

// DefaultCases returns the two standard frozen fixture cases for benchmark execution.
func DefaultCases() []Case {
	c1, err1 := buildCaseAuth()
	if err1 != nil {
		panic(fmt.Sprintf("failed to build case_auth: %v", err1))
	}
	c2, err2 := buildCaseOrder()
	if err2 != nil {
		panic(fmt.Sprintf("failed to build case_order: %v", err2))
	}
	return []Case{c1, c2}
}

func buildCaseAuth() (Case, error) {
	snap, err := evidence.Freeze("snap_auth_service", []evidence.Unit{
		{ID: "REQ-1", Kind: evidence.UnitRequirement, Text: "All API endpoints must authenticate callers using bearer JWT."},
		{ID: "REQ-2", Kind: evidence.UnitRequirement, Text: "Password reset tokens must expire after 15 minutes."},
		{ID: "COMP-1", Kind: evidence.UnitComponent, Text: "Auth service validates signatures against public keys."},
		{ID: "FLOW-1", Kind: evidence.UnitFlow, Text: "Client submits login credentials, receiving refresh token."},
		{ID: "FLOW-2", Kind: evidence.UnitFlow, Text: "Unauthenticated health endpoint bypasses auth check."},
		{ID: "SEC-1", Kind: evidence.UnitConstraint, Text: "Admin endpoints require role=admin claim."},
	})
	if err != nil {
		return Case{}, err
	}

	defects := []SeededDefect{
		{
			ID:        "D1",
			Kind:      DefectWeakenedAuth,
			UnitIDs:   []string{"REQ-1", "FLOW-2"},
			Rationale: "Health endpoint bypasses auth check without rate limiting",
		},
		{
			ID:        "D2",
			Kind:      DefectDroppedRequirement,
			UnitIDs:   []string{"REQ-2"},
			Rationale: "Password reset expiration requirement is not implemented in flow",
		},
		{
			ID:        "D3",
			Kind:      DefectMissingFailurePath,
			UnitIDs:   []string{"FLOW-1"},
			Rationale: "Invalid credential rejection and lockout path missing from flow",
		},
	}

	return Case{
		ID:       "case_auth",
		Snapshot: snap,
		Defects:  defects,
	}, nil
}

func buildCaseOrder() (Case, error) {
	snap, err := evidence.Freeze("snap_order_pipeline", []evidence.Unit{
		{ID: "REQ-10", Kind: evidence.UnitRequirement, Text: "Orders must be idempotently processed within 5 seconds."},
		{ID: "REQ-11", Kind: evidence.UnitRequirement, Text: "Order status transitions from pending to confirmed or cancelled."},
		{ID: "COMP-10", Kind: evidence.UnitComponent, Text: "Order processor reads messages from partitioned Kafka topic."},
		{ID: "DATA-10", Kind: evidence.UnitDataRule, Text: "Database requires total_amount to equal sum of item prices."},
		{ID: "FLOW-10", Kind: evidence.UnitFlow, Text: "Payment service deducts inventory before charging card."},
	})
	if err != nil {
		return Case{}, err
	}

	defects := []SeededDefect{
		{
			ID:        "D1",
			Kind:      DefectContradiction,
			UnitIDs:   []string{"REQ-10", "FLOW-10"},
			Rationale: "5s processing deadline contradicted by synchronous payment wait",
		},
		{
			ID:        "D2",
			Kind:      DefectMissingFailurePath,
			UnitIDs:   []string{"FLOW-10"},
			Rationale: "Inventory rollback path missing if payment charge fails",
		},
		{
			ID:        "D3",
			Kind:      DefectContradiction,
			UnitIDs:   []string{"REQ-11", "DATA-10"},
			Rationale: "Order status cancel logic allows negative totals",
		},
	}

	return Case{
		ID:       "case_order",
		Snapshot: snap,
		Defects:  defects,
	}, nil
}

// ScriptedFakeProviderForRun returns a deterministic fake provider for a specific (case, arm, repeat).
func ScriptedFakeProviderForRun(caseID string, arm Arm, repeat int) *fake.FakeProvider {
	script := make(map[domain.Role][]fake.ScriptedCall)

	if caseID == "case_auth" {
		switch arm {
		case ArmStructured:
			script[domain.Role("structured")] = []fake.ScriptedCall{
				{Body: scriptedAuthStructuredBody(repeat)},
			}
		case ArmRoles:
			script[domain.RoleRequirements] = []fake.ScriptedCall{{Body: authReqBody()}}
			script[domain.RoleArchitecture] = []fake.ScriptedCall{{Body: authArchBody()}}
			script[domain.RoleQA] = []fake.ScriptedCall{{Body: authQABody()}}
			script[domain.RoleSecurity] = []fake.ScriptedCall{{Body: authSecBody()}}
		case ArmGeneric:
			script[domain.Role("generic")] = []fake.ScriptedCall{
				{Body: authGenericCall1Body()},
				{Body: authGenericCall2Body()},
				{Body: authGenericCall3Body()},
				{Body: authGenericCall4Body()},
			}
		case ArmFreeform:
			script[domain.Role("freeform")] = []fake.ScriptedCall{
				{Body: authFreeformBody()},
			}
		}
	} else {
		// case_order
		switch arm {
		case ArmStructured:
			script[domain.Role("structured")] = []fake.ScriptedCall{
				{Body: scriptedOrderStructuredBody(repeat)},
			}
		case ArmRoles:
			script[domain.RoleRequirements] = []fake.ScriptedCall{{Body: orderReqBody()}}
			script[domain.RoleArchitecture] = []fake.ScriptedCall{{Body: orderArchBody()}}
			script[domain.RoleQA] = []fake.ScriptedCall{{Body: orderQABody()}}
			script[domain.RoleSecurity] = []fake.ScriptedCall{{Body: orderSecBody()}}
		case ArmGeneric:
			script[domain.Role("generic")] = []fake.ScriptedCall{
				{Body: orderGenericCall1Body()},
				{Body: orderGenericCall2Body()},
				{Body: orderGenericCall3Body()},
				{Body: orderGenericCall4Body()},
			}
		case ArmFreeform:
			script[domain.Role("freeform")] = []fake.ScriptedCall{
				{Body: orderFreeformBody()},
			}
		}
	}

	return fake.NewFakeProvider(script)
}

// ---- Case 1 (case_auth) Scripted Responses ----

func scriptedAuthStructuredBody(repeat int) string {
	switch repeat {
	case 1:
		// 4 findings: 2 hits (D1, D3), 1 false (COMP-1), 1 duplicate (REQ-1)
		return `{"findings":[
			{"id":"F1","kind":"existing","severity":"high","category":"auth","issue":"weakened auth check","recommendation":"require bearer JWT","basis_refs":["REQ-1"]},
			{"id":"F2","kind":"missing","severity":"medium","category":"qa","issue":"missing failure path on invalid credentials","recommendation":"add lockout","anchor_ref":"FLOW-1"},
			{"id":"F3","kind":"existing","severity":"low","category":"arch","issue":"public key caching not specified","recommendation":"document key caching","basis_refs":["COMP-1"]},
			{"id":"F4","kind":"existing","severity":"high","category":"auth","issue":"weakened auth check duplicate","recommendation":"require bearer JWT","basis_refs":["REQ-1"]}
		]}`
	case 2:
		// 2 findings: 1 hit (D1), 1 false (COMP-1), 0 duplicates
		return `{"findings":[
			{"id":"F1","kind":"existing","severity":"high","category":"auth","issue":"weakened auth check","recommendation":"require bearer JWT","basis_refs":["REQ-1"]},
			{"id":"F2","kind":"existing","severity":"low","category":"arch","issue":"public key caching not specified","recommendation":"document key caching","basis_refs":["COMP-1"]}
		]}`
	default:
		// 2 findings: 2 hits (D1, D3), 0 false, 0 duplicates
		return `{"findings":[
			{"id":"F1","kind":"existing","severity":"high","category":"auth","issue":"weakened auth check","recommendation":"require bearer JWT","basis_refs":["REQ-1"]},
			{"id":"F2","kind":"missing","severity":"medium","category":"qa","issue":"missing failure path on invalid credentials","recommendation":"add lockout","anchor_ref":"FLOW-1"}
		]}`
	}
}

func authReqBody() string {
	return `{"findings":[
		{"id":"R1","kind":"existing","severity":"medium","category":"requirements","issue":"password reset token expiration not specified in flow","recommendation":"add expiration check","basis_refs":["REQ-2"]}
	]}`
}

func authArchBody() string {
	return `{"findings":[
		{"id":"A1","kind":"existing","severity":"high","category":"architecture","issue":"bearer JWT bypass allows unauthenticated access","recommendation":"enforce JWT on all endpoints","basis_refs":["REQ-1"]}
	]}`
}

func authQABody() string {
	return `{"findings":[
		{"id":"Q1","kind":"missing","severity":"high","category":"qa","issue":"no failure path for invalid credential login","recommendation":"define rejection behavior","anchor_ref":"FLOW-1"}
	]}`
}

func authSecBody() string {
	return `{"findings":[
		{"id":"S1","kind":"existing","severity":"critical","category":"security","issue":"health endpoint bypasses authentication entirely","recommendation":"add rate limit or auth check","basis_refs":["FLOW-2"]}
	]}`
}

func authGenericCall1Body() string {
	return `{"findings":[
		{"id":"G1","kind":"existing","severity":"high","category":"generic","issue":"weakened auth check on api endpoints","recommendation":"enforce JWT","basis_refs":["REQ-1"]}
	]}`
}

func authGenericCall2Body() string {
	return `{"findings":[
		{"id":"G2","kind":"missing","severity":"medium","category":"generic","issue":"missing failure path for credential rejection","recommendation":"add lockout","anchor_ref":"FLOW-1"}
	]}`
}

func authGenericCall3Body() string {
	// Duplicate of Call 1
	return `{"findings":[
		{"id":"G3","kind":"existing","severity":"high","category":"generic","issue":"weakened auth check on api endpoints","recommendation":"enforce JWT","basis_refs":["REQ-1"]}
	]}`
}

func authGenericCall4Body() string {
	// False finding
	return `{"findings":[
		{"id":"G4","kind":"existing","severity":"low","category":"generic","issue":"public key rotation interval omitted","recommendation":"document rotation","basis_refs":["COMP-1"]}
	]}`
}

func authFreeformBody() string {
	return `{"findings":[
		{"id":"FF1","kind":"existing","severity":"high","category":"freeform","issue":"weakened auth check on REQ-1","recommendation":"fix auth","basis_refs":["REQ-1"]},
		{"id":"FF2","kind":"missing","severity":"medium","category":"freeform","issue":"missing failure path on FLOW-1","recommendation":"add lockout","anchor_ref":"FLOW-1"},
		{"id":"FF3","kind":"existing","severity":"low","category":"freeform","issue":"admin endpoint role check missing test SEC-1","recommendation":"add test","basis_refs":["SEC-1"]}
	]}`
}

// ---- Case 2 (case_order) Scripted Responses ----

func scriptedOrderStructuredBody(repeat int) string {
	switch repeat {
	case 1:
		// 4 findings: 2 hits (D1, D2), 1 false (COMP-10), 1 duplicate (REQ-10, FLOW-10)
		return `{"findings":[
			{"id":"F10","kind":"conflicting","severity":"high","category":"order","issue":"5s timeout contradicts synchronous payment wait","recommendation":"use async flow","basis_refs":["REQ-10","FLOW-10"]},
			{"id":"F11","kind":"missing","severity":"medium","category":"qa","issue":"missing inventory rollback if charge fails","recommendation":"add compensation flow","anchor_ref":"FLOW-10"},
			{"id":"F12","kind":"existing","severity":"low","category":"arch","issue":"kafka partition key strategy omitted","recommendation":"document partitioning","basis_refs":["COMP-10"]},
			{"id":"F13","kind":"conflicting","severity":"high","category":"order","issue":"5s timeout contradicts synchronous payment wait duplicate","recommendation":"use async flow","basis_refs":["REQ-10","FLOW-10"]}
		]}`
	case 2:
		// 2 findings: 1 hit (D1), 1 false (COMP-10), 0 duplicates
		return `{"findings":[
			{"id":"F10","kind":"conflicting","severity":"high","category":"order","issue":"5s timeout contradicts synchronous payment wait","recommendation":"use async flow","basis_refs":["REQ-10","FLOW-10"]},
			{"id":"F12","kind":"existing","severity":"low","category":"arch","issue":"kafka partition key strategy omitted","recommendation":"document partitioning","basis_refs":["COMP-10"]}
		]}`
	default:
		// 2 findings: 2 hits (D1, D2), 0 false, 0 duplicates
		return `{"findings":[
			{"id":"F10","kind":"conflicting","severity":"high","category":"order","issue":"5s timeout contradicts synchronous payment wait","recommendation":"use async flow","basis_refs":["REQ-10","FLOW-10"]},
			{"id":"F11","kind":"missing","severity":"medium","category":"qa","issue":"missing inventory rollback if charge fails","recommendation":"add compensation flow","anchor_ref":"FLOW-10"}
		]}`
	}
}

func orderReqBody() string {
	return `{"findings":[
		{"id":"R10","kind":"conflicting","severity":"high","category":"requirements","issue":"cancelled order state contradicts total_amount rule","recommendation":"clarify refund constraint","basis_refs":["REQ-11","DATA-10"]}
	]}`
}

func orderArchBody() string {
	return `{"findings":[
		{"id":"A10","kind":"conflicting","severity":"high","category":"architecture","issue":"5s processing deadline contradicted by synchronous payment","recommendation":"switch to async event","basis_refs":["REQ-10","FLOW-10"]}
	]}`
}

func orderQABody() string {
	return `{"findings":[
		{"id":"Q10","kind":"missing","severity":"high","category":"qa","issue":"missing inventory rollback failure path","recommendation":"define rollback on charge failure","anchor_ref":"FLOW-10"}
	]}`
}

func orderSecBody() string {
	return `{"findings":[
		{"id":"S10","kind":"existing","severity":"medium","category":"security","issue":"payment service deducts inventory before auth check","recommendation":"verify caller before deduction","basis_refs":["FLOW-10"]}
	]}`
}

func orderGenericCall1Body() string {
	return `{"findings":[
		{"id":"G10","kind":"conflicting","severity":"high","category":"generic","issue":"5s processing deadline contradicted by payment flow","recommendation":"use async flow","basis_refs":["REQ-10","FLOW-10"]}
	]}`
}

func orderGenericCall2Body() string {
	return `{"findings":[
		{"id":"G11","kind":"missing","severity":"medium","category":"generic","issue":"missing inventory rollback when charge fails","recommendation":"add compensation","anchor_ref":"FLOW-10"}
	]}`
}

func orderGenericCall3Body() string {
	// Duplicate of Call 1
	return `{"findings":[
		{"id":"G12","kind":"conflicting","severity":"high","category":"generic","issue":"5s processing deadline contradicted by payment flow","recommendation":"use async flow","basis_refs":["REQ-10","FLOW-10"]}
	]}`
}

func orderGenericCall4Body() string {
	// False finding
	return `{"findings":[
		{"id":"G13","kind":"existing","severity":"low","category":"generic","issue":"kafka partition strategy not specified","recommendation":"document key","basis_refs":["COMP-10"]}
	]}`
}

func orderFreeformBody() string {
	return `{"findings":[
		{"id":"FF10","kind":"conflicting","severity":"high","category":"freeform","issue":"5s deadline contradicts FLOW-10","recommendation":"use async","basis_refs":["REQ-10","FLOW-10"]},
		{"id":"FF11","kind":"missing","severity":"medium","category":"freeform","issue":"missing rollback on FLOW-10","recommendation":"add compensation","anchor_ref":"FLOW-10"},
		{"id":"FF12","kind":"existing","severity":"low","category":"freeform","issue":"kafka partition key strategy omitted COMP-10","recommendation":"document key","basis_refs":["COMP-10"]}
	]}`
}

// Runner runs a benchmark across cases and arms.
type RunnerOptions struct {
	Repeats         int
	Arms            []Arm
	Provider        provider.Provider
	ProviderFactory func(caseID string, arm Arm, repeat int) (provider.Provider, error)
}

type Runner struct {
	opts RunnerOptions
}

// NewRunner creates a new benchmark runner with options.
func NewRunner(opts RunnerOptions) *Runner {
	if opts.Repeats <= 0 {
		opts.Repeats = 3
	}
	if len(opts.Arms) == 0 {
		opts.Arms = []Arm{ArmFreeform, ArmStructured, ArmRoles, ArmGeneric}
	}
	if opts.ProviderFactory == nil && opts.Provider == nil {
		opts.ProviderFactory = func(caseID string, arm Arm, repeat int) (provider.Provider, error) {
			return ScriptedFakeProviderForRun(caseID, arm, repeat), nil
		}
	}
	return &Runner{opts: opts}
}

// Run executes the full benchmark over the provided cases and returns aggregated ArmScores.
func (r *Runner) Run(ctx context.Context, cases []Case) ([]ArmScore, error) {
	var scores []ArmScore

	for _, c := range cases {
		for _, arm := range r.opts.Arms {
			runs := make([]RunResult, 0, r.opts.Repeats)

			for repeat := 1; repeat <= r.opts.Repeats; repeat++ {
				var p provider.Provider
				if r.opts.ProviderFactory != nil {
					var err error
					p, err = r.opts.ProviderFactory(c.ID, arm, repeat)
					if err != nil {
						return nil, fmt.Errorf("provider factory failed for %s/%s repeat %d: %w", c.ID, arm, repeat, err)
					}
				} else {
					p = r.opts.Provider
				}

				res, err := ExecuteArm(ctx, p, arm, c, repeat)
				if err != nil {
					return nil, fmt.Errorf("execute arm %s on case %s repeat %d failed: %w", arm, c.ID, repeat, err)
				}
				runs = append(runs, res)
			}

			armScore := AggregateArm(arm, c, runs)
			scores = append(scores, armScore)
		}
	}

	return scores, nil
}
