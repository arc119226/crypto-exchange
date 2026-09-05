package app

import (
	"fmt"
	"strings"
)

// Role is one of the process roles of the single exchange binary
// (docs/plan-v1.0.md §5.1).
type Role string

// Roles.
const (
	RoleAPI    Role = "api"
	RoleEngine Role = "engine"
	RoleChain  Role = "chain"
	RoleSigner Role = "signer"
	RoleStream Role = "stream"
	RoleAdmin  Role = "admin"
	RoleWorker Role = "worker"
	RoleAll    Role = "all"
)

// AllRoles is the expansion of "all", in start order.
var AllRoles = []Role{RoleAPI, RoleEngine, RoleChain, RoleSigner, RoleStream, RoleAdmin, RoleWorker}

func (r Role) String() string { return string(r) }

// ParseRoles parses a comma-separated role list. "all" expands to AllRoles.
func ParseRoles(s string) ([]Role, error) {
	seen := map[Role]bool{}
	var out []Role
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		r := Role(strings.ToLower(part))
		if r == RoleAll {
			for _, a := range AllRoles {
				if !seen[a] {
					seen[a] = true
					out = append(out, a)
				}
			}
			continue
		}
		if !validRole(r) {
			return nil, fmt.Errorf("unknown role %q (valid: %s, all)", part, joinRoles(AllRoles))
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no roles given (valid: %s, all)", joinRoles(AllRoles))
	}
	return out, nil
}

func validRole(r Role) bool {
	for _, a := range AllRoles {
		if a == r {
			return true
		}
	}
	return false
}

func joinRoles(rs []Role) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = string(r)
	}
	return strings.Join(parts, ",")
}

// RolesLabel is the "role" attribute used in logs and metrics.
func RolesLabel(rs []Role) string {
	if len(rs) == len(AllRoles) {
		return string(RoleAll)
	}
	return joinRoles(rs)
}
