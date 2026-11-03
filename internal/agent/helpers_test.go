package agent

import (
	"encoding/json"

	"idp-host/internal/canonical"
)

func canonicalDigest(raw json.RawMessage) (string, error) { return canonical.Digest(raw) }
