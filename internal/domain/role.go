package domain

// Role is one of the four fixed specialist reviewers in v1.
//
// The four perspectives are canonical: Requirements, Architecture, QA, Security.
// Placeholder role names (clarity, consistency, risk, verifiability) are not
// valid role identifiers.
type Role string

const (
	RoleRequirements Role = "requirements"
	RoleArchitecture Role = "architecture"
	RoleQA           Role = "qa"
	RoleSecurity     Role = "security"
)

// Roles is the canonical reviewer set in dispatch order (lowest role order first).
var Roles = []Role{RoleRequirements, RoleArchitecture, RoleQA, RoleSecurity}

// RoleCount is the fixed number of reviewer roles in v1.
const RoleCount = 4

// RoleOrder returns the canonical rank of a role, or -1 when the role is unknown.
func RoleOrder(r Role) int {
	for i, known := range Roles {
		if known == r {
			return i
		}
	}
	return -1
}

// IsValidRole reports whether r is one of the four canonical roles.
func IsValidRole(r Role) bool {
	return RoleOrder(r) >= 0
}

// String returns the canonical role identifier.
func (r Role) String() string {
	return string(r)
}
