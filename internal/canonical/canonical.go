package canonical

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Canonical encodes v as canonical JSON for digests: object keys sorted, no insignificant
// whitespace, no HTML escaping, numbers kept as written. The value is first re-decoded generically
// so the result does not depend on Go struct field order; producer and verifier therefore agree on
// any JSON document, not only on documents built from the same struct. (Canonical within this
// system; not RFC 8785, which would also normalize number formatting.)
func Canonical(v any) ([]byte, error) {
	raw, ok := v.(json.RawMessage)
	if !ok {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil { // maps encode with sorted keys
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Digest is "sha256:<hex>" of the canonical encoding of v.
func Digest(v any) (string, error) {
	b, err := Canonical(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
