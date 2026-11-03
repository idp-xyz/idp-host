// Package authorization holds the authenticated caller and the HOST's role model. Decisions are
// made on tenant, subject and roles only; everything else on the principal is audit information.
package authorization

import "time"

// Roles. An administrator manages agent definitions and publishes releases; an approver signs
// releases (never their own); a user owns sessions, threads and runs.
const (
	RoleAdmin    = "host-admin"
	RoleApprover = "host-approver"
	RoleUser     = "host-user"
)

// Principal is an authenticated caller.
type Principal struct {
	Subject string
	Tenant  string
	Roles   []string
	Method  string    // "jwt" or "dev"
	Expires time.Time // credential expiry
	// Client is the OAuth client the token was issued to (azp); Actor the party acting for the
	// subject in a delegated token (RFC 8693 act claim). Audit only.
	Client string
	Actor  string
}

// Has reports whether the principal holds role.
func (p Principal) Has(role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}
