package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Caller options are carried in AG-UI forwardedProps under the "idp.host/" prefix (ADR-0001 D19).
// Other keys are the caller's business and are kept untouched; an unknown "idp.host/" key is an
// error, so a caller never believes an option took effect when it did not.
const (
	OptExecutionRef    = "idp.host/executionRef"
	OptToolExecution   = "idp.host/toolExecution"
	OptReleaseDigest   = "idp.host/releaseDigest"
	OptMaxOutputTokens = "idp.host/maxOutputTokens"
)

// Options are the parsed caller options.
type Options struct {
	ExecutionRef    string // reference to the caller's execution (EER), recorded on the run
	ToolExecution   string // "" or ToolExecutionCaller
	ReleaseDigest   string // run exactly this approved release of the agent
	MaxOutputTokens int
}

var (
	refRE    = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ParseOptions reads the idp.host/ options of forwardedProps (absent or null = no options).
func ParseOptions(raw json.RawMessage) (Options, error) {
	var o Options
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return o, nil
	}
	var props map[string]json.RawMessage
	if err := json.Unmarshal(raw, &props); err != nil {
		return o, fmt.Errorf("forwardedProps must be an object")
	}
	for key, value := range props {
		if !strings.HasPrefix(key, "idp.host/") {
			continue
		}
		switch key {
		case OptExecutionRef:
			if err := json.Unmarshal(value, &o.ExecutionRef); err != nil || !refRE.MatchString(o.ExecutionRef) {
				return o, fmt.Errorf("%s must be 1-256 visible ASCII characters", key)
			}
		case OptToolExecution:
			if err := json.Unmarshal(value, &o.ToolExecution); err != nil || (o.ToolExecution != "server" && o.ToolExecution != ToolExecutionCaller) {
				return o, fmt.Errorf("%s must be server or caller", key)
			}
			if o.ToolExecution == "server" {
				o.ToolExecution = ""
			}
		case OptReleaseDigest:
			if err := json.Unmarshal(value, &o.ReleaseDigest); err != nil || !digestRE.MatchString(o.ReleaseDigest) {
				return o, fmt.Errorf("%s must be sha256:<64 hex>", key)
			}
		case OptMaxOutputTokens:
			if err := json.Unmarshal(value, &o.MaxOutputTokens); err != nil || o.MaxOutputTokens < 1 || o.MaxOutputTokens > 1<<20 {
				return o, fmt.Errorf("%s must be an integer in [1, 1048576]", key)
			}
		default:
			return o, fmt.Errorf("unknown option %s", key)
		}
	}
	return o, nil
}
