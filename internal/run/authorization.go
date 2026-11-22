package run

// The marker with which a tool result says that the member has not authorized the third-party service the tool reaches
// (ADR-0001 D61; platform plan idp-mcp-bridge-oauth, section 2). It is data about the run and is never given to a model.

import (
	"encoding/json"
	"regexp"
)

// AuthorizationRequiredKey is the key of structuredContent that carries {"connection": "<id>"}. The value is an id and never an
// address: the Workspace builds its own link from the id.
const AuthorizationRequiredKey = "io.idp.telos/authorizationRequired"

// MaxConnectionID bounds the id of a connection, in bytes.
const MaxConnectionID = 64

var connectionIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ParseAuthorizationRequired reads the marker from a tool result's structuredContent. It returns the connection id when the
// marker is there in the documented shape: an object with a string "connection" of letters, digits, '-' and '_', at most
// MaxConnectionID long. Anything else (another type, a longer or other id, an address, extra fields only) gives false and is
// ignored: the tool's own result goes to the model as before. Only the id is read, so nothing else in the marker, an address
// least of all, can reach an event.
func ParseAuthorizationRequired(structured json.RawMessage) (connection string, ok bool) {
	if len(structured) == 0 {
		return "", false
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(structured, &doc) != nil {
		return "", false
	}
	raw, present := doc[AuthorizationRequiredKey]
	if !present {
		return "", false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return "", false
	}
	if json.Unmarshal(m["connection"], &connection) != nil || !connectionIDRE.MatchString(connection) {
		return "", false
	}
	return connection, true
}
