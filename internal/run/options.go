package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Caller options are carried in AG-UI forwardedProps under the "idp.host/" prefix (ADR-0001 D19).
// Other keys are the caller's business and are kept untouched; an unknown "idp.host/" key is an
// error, so a caller never believes an option took effect when it did not.
const (
	OptExecutionRef    = "idp.host/executionRef"
	OptToolExecution   = "idp.host/toolExecution"
	OptReleaseDigest   = "idp.host/releaseDigest"
	OptMaxOutputTokens = "idp.host/maxOutputTokens"
	OptAttachments     = "idp.host/attachments"
	OptDelivery        = "idp.host/delivery"
)

// MaxAttachments bounds the files one run may be given.
const MaxAttachments = 10

// Attachment is a member's file the caller hands to a run (platform ADR-0002 F2, F3; D37): what the model is told about
// it, and the read ticket the HOST puts in place of attachment:N in a file-taking tool's arguments. The ticket is sealed
// apart from the run's input and never reaches the model, the event log or the gateway's records.
type Attachment struct {
	Ref       string `json:"ref"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Ticket    string `json:"ticket,omitempty"`
}

// Delivery lets a run hand files to its owner: the write ticket the HOST puts in place of output.
type Delivery struct {
	Ticket string `json:"ticket"`
}

// Options are the parsed caller options. Their JSON form is the contract object RunOptions
// (idp.telos.host 1.2.0): the idp.host/ keys of forwardedProps.
type Options struct {
	ExecutionRef    string       `json:"idp.host/executionRef,omitempty"`    // reference to the caller's execution (EER), recorded on the run
	ToolExecution   string       `json:"idp.host/toolExecution,omitempty"`   // "" (server) or ToolExecutionCaller
	ReleaseDigest   string       `json:"idp.host/releaseDigest,omitempty"`   // run exactly this approved release of the agent
	MaxOutputTokens int          `json:"idp.host/maxOutputTokens,omitempty"` // caps the release's model output limit
	Attachments     []Attachment `json:"idp.host/attachments,omitempty"`     // the member's files for this run (D37)
	Delivery        *Delivery    `json:"idp.host/delivery,omitempty"`        // the run may deliver files to its owner (D37)
}

var (
	refRE    = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	fileRef  = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	ticketRE = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)
	mediaRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]{0,126}/[a-z0-9][a-z0-9!#$&^_.+-]{0,126}$`)
)

// validName: a file name the model may be told, one line of printable characters.
func validName(s string) bool {
	if s == "" || len(s) > 255 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029 {
			return false
		}
	}
	return true
}

func validTicket(s string) bool { return len(s) <= 8192 && ticketRE.MatchString(s) }

// strict decodes a JSON object into v, refusing unknown fields and trailing data.
func strict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data")
	}
	return nil
}

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
		case OptAttachments:
			var list []json.RawMessage
			if err := json.Unmarshal(value, &list); err != nil || len(list) == 0 || len(list) > MaxAttachments {
				return o, fmt.Errorf("%s must be an array of 1-%d files", key, MaxAttachments)
			}
			seen := map[string]bool{}
			for i, item := range list {
				var a Attachment
				if err := strict(item, &a); err != nil || !fileRef.MatchString(a.Ref) || !validName(a.Name) || !mediaRE.MatchString(a.MediaType) ||
					a.Size < 0 || a.Size > 1<<31 || !sha256RE.MatchString(a.SHA256) || !validTicket(a.Ticket) || seen[a.Ref] {
					return o, fmt.Errorf("%s[%d] must be {ref, name, mediaType, size, sha256, ticket} with a distinct ref, a one-line name and a signed ticket", key, i)
				}
				seen[a.Ref] = true
				o.Attachments = append(o.Attachments, a)
			}
		case OptDelivery:
			var d Delivery
			if err := strict(value, &d); err != nil || !validTicket(d.Ticket) {
				return o, fmt.Errorf("%s must be {ticket} with a signed ticket", key)
			}
			o.Delivery = &d
		default:
			return o, fmt.Errorf("unknown option %s", key)
		}
	}
	return o, nil
}

// WithoutFileOptions returns forwardedProps without the file options: their tickets are sealed apart from the run's
// input (D37), and the attachments' description is kept in Input.Attachments.
func WithoutFileOptions(raw json.RawMessage) json.RawMessage {
	var props map[string]json.RawMessage
	if json.Unmarshal(raw, &props) != nil {
		return raw
	}
	if _, a := props[OptAttachments]; !a {
		if _, d := props[OptDelivery]; !d {
			return raw
		}
	}
	delete(props, OptAttachments)
	delete(props, OptDelivery)
	out, _ := json.Marshal(props)
	return out
}

// FileTickets are a run's sealed file tickets: Read[i] is the ticket of attachment:i+1, Write the delivery ticket.
type FileTickets struct {
	Read  []string `json:"read,omitempty"`
	Write string   `json:"write,omitempty"`
}

// Tickets returns the options' tickets (nil when there are none).
func (o Options) Tickets() *FileTickets {
	if len(o.Attachments) == 0 && o.Delivery == nil {
		return nil
	}
	t := &FileTickets{}
	for _, a := range o.Attachments {
		t.Read = append(t.Read, a.Ticket)
	}
	if o.Delivery != nil {
		t.Write = o.Delivery.Ticket
	}
	return t
}
