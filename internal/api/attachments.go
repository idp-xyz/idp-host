package api

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/run"
)

// FilePolicy decides which of a member's files a run may be given (ADR-0001 D43; platform ADR-0002 F8): a file
// classified above the tenant's ceiling does not reach a run whose release may use a model outside the enterprise.
type FilePolicy struct {
	// Local names the providers the operator declared to be the enterprise's own (external: false). Every other
	// provider, a remote A2A agent included, counts as external.
	Local map[string]bool
	// Ceiling returns the highest sensitivity that may reach an external model for a tenant; nil means internal.
	Ceiling func(tenant string) string
	// ExternalTools are the gateways' external-tool patterns by gateway name (D44): a release that may call one of them is
	// screened like one that may use an external model.
	ExternalTools map[string][]string
	// Release loads the release of a local sub-agent that a release embeds, by agent id and digest (D48). Nil, or an
	// error, means the sub-agent cannot be judged and counts as sending content out.
	Release func(ctx context.Context, tenant, agentID, digest string) (*agent.Document, error)
}

// ContextAttachmentsWithheld is the caller-context entry that tells the model which files it was not given, so that it
// can say so instead of looking for them.
const ContextAttachmentsWithheld = "idp.host/attachments.withheld"

// externalModels lists the providers outside the enterprise that a release may send a run's content to: its model, the
// fallbacks and the routing candidates, and remote A2A subagents (named by their card URL). Sorted, without repeats.
func (p FilePolicy) externalModels(doc *agent.Document) []string {
	seen := map[string]bool{}
	add := func(provider string) {
		if provider != "" && !p.Local[provider] {
			seen[provider] = true
		}
	}
	m := doc.Definition.Model
	add(m.Provider)
	for _, f := range m.Fallbacks {
		add(f.Provider)
	}
	if m.Routing != nil {
		for _, c := range m.Routing.Candidates {
			add(c.Provider)
		}
	}
	for _, s := range doc.Definition.Subagents {
		if s.A2A != nil {
			seen["a2a:"+s.A2A.CardURL] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Catalogue tells what a gateway lists for the caller who creates a run: tool name to whether the gateway marks the tool
// as sending content outside the enterprise (D51). An error means the HOST cannot know. A nil Catalogue is not consulted
// (only the operator's patterns count); the HOST's main always sets one.
type Catalogue func(gateway string) (map[string]bool, error)

// hasExternalTools reports whether the release may call a tool that the operator's patterns call outward (D44; platform
// ADR-0003 D7): any tool of its effective policy (skills included) matches the externalTools patterns of its gateway, or
// the policy allows "*", because the gateway's catalogue is not known when the run is created.
func (p FilePolicy) hasExternalTools(doc *agent.Document) bool {
	tools := doc.Effective().Tools
	return tools.MatchesAny(p.ExternalTools[tools.Gateway])
}

// toolVerdict is what the HOST knows at run creation about the tools of one release (D44, D51).
type toolVerdict struct {
	outward bool
	// marked are the tools of the release's allow list that the gateway lists as external now, sorted; set only when the
	// operator's patterns did not already make the release outward, so that it is the marker that did.
	marked []string
	// unknown names the gateway whose catalogue could not be read: the release counts as outward (fail closed).
	unknown string
}

// judgeTools decides whether the tools of doc may send content outside the enterprise: the operator's patterns first
// (no call is needed then), otherwise what the gateway marks in the catalogue it lists to this caller now. A release
// without tools asks nothing. A catalogue that cannot be read makes the release outward. The allow list is names or "*",
// so "*" matches every tool of the catalogue now; a tool that is external later is the turn's to refuse (D51).
func (p FilePolicy) judgeTools(doc *agent.Document, look Catalogue) toolVerdict {
	if p.hasExternalTools(doc) {
		return toolVerdict{outward: true}
	}
	tools := doc.Effective().Tools
	if look == nil || tools.Gateway == "" || len(tools.Allow) == 0 {
		return toolVerdict{}
	}
	cat, err := look(tools.Gateway)
	if err != nil {
		return toolVerdict{outward: true, unknown: tools.Gateway}
	}
	var v toolVerdict
	for name, external := range cat {
		if external && !agent.Reserved(name) && tools.Allows(name) {
			v.marked = append(v.marked, name)
		}
	}
	sort.Strings(v.marked)
	v.outward = len(v.marked) > 0
	return v
}

// maxMarkedNamed bounds how many marked tools a reason names.
const maxMarkedNamed = 5

func namedTools(names []string) string {
	if len(names) > maxMarkedNamed {
		return strings.Join(names[:maxMarkedNamed], ", ") + fmt.Sprintf(" and %d more", len(names)-maxMarkedNamed)
	}
	return strings.Join(names, ", ")
}

// reasons are the entries of Outward.Via that a release's own tools give (D51): which tools the gateway marked, or that
// its catalogue could not be read. The operator's patterns give none, as before.
func (v toolVerdict) reasons() []string {
	switch {
	case v.unknown != "":
		return []string{"gateway " + v.unknown + ": its tool catalogue could not be read when the run was created, so its tools count as sending content out"}
	case len(v.marked) > 0:
		return []string{"tools marked by the gateway as sending content outside the enterprise: " + namedTools(v.marked)}
	}
	return nil
}

// Outward is what the HOST decided at run creation about a run's content leaving the enterprise (D44, D48, D51).
type Outward struct {
	// Trigger says why the run is screened against the ceiling: "" when it is not. Parts joined by "+".
	Trigger string
	// Models are the providers outside the enterprise the release itself may use (D43).
	Models []string
	// Tools: the release itself may call a tool that sends content outside the enterprise (D44): the operator's patterns
	// say so, or the gateway marks one of its allowed tools (D51), or the gateway's catalogue could not be read.
	Tools bool
	// Via names the sub-agents (by path of names) and remote agents that make the run count as outward and why, so that
	// an operator can tell why an agent no longer sees confidential data (D48). It also says when it is the gateway's
	// marker (which tools), or an unreadable catalogue, that makes the run outward (D51). Never any content.
	Via []string
}

// outward decides whether a run of doc may send content outside the enterprise. This is the one place it is computed
// (D48, D51): the release's own models and tools as D43, D44 and D51 decided, any local sub-agent it can reach that
// qualifies in the same way, and any remote A2A agent. A task the parent writes for a sub-agent is free text that cannot
// be screened, so the parent must never be given what it could write into one. look says what the gateways list for
// this caller; nil means the marker is not consulted.
func (p FilePolicy) outward(ctx context.Context, tenant string, doc *agent.Document, look Catalogue) Outward {
	v := p.judgeTools(doc, look)
	o := Outward{Models: p.externalModels(doc), Tools: v.outward}
	var why []string
	if len(o.Models) > 0 {
		why = append(why, agui.ScreenExternalModel)
	}
	if o.Tools {
		why = append(why, agui.ScreenExternalTools)
	}
	o.Via = v.reasons()
	if sub := p.viaSubagents(ctx, tenant, doc, look); len(sub) > 0 {
		o.Via = append(o.Via, sub...)
		why = append(why, agui.ScreenSubagent)
	}
	o.Trigger = strings.Join(why, "+")
	return o
}

// screen splits the attachments of a run into those it gets and those it does not. Nothing is withheld from a run that
// is not outward, or below the ceiling.
func (p FilePolicy) screen(tenant string, o Outward, atts []run.Attachment) (kept, withheld []run.Attachment, ceiling string) {
	ceiling = "internal"
	if p.Ceiling != nil {
		ceiling = p.Ceiling(tenant)
	}
	if len(atts) == 0 || o.Trigger == "" {
		return atts, nil, ceiling
	}
	limit, _ := run.SensitivityRank(ceiling)
	for _, a := range atts {
		if rank, _ := run.SensitivityRank(a.Sensitivity); rank > limit {
			withheld = append(withheld, a)
		} else {
			kept = append(kept, a)
		}
	}
	return kept, withheld, ceiling
}

// withheldEvents records each withheld file on the run's stream: which file, how it is classified, and the ceiling.
func withheldEvents(withheld []run.Attachment, ceiling string, o Outward) []agui.Event {
	var out []agui.Event
	for _, a := range withheld {
		payload := map[string]any{"ref": a.Ref, "name": a.Name, "sensitivity": levelOf(a),
			"ceiling": ceiling, "externalModels": o.Models, "trigger": o.Trigger}
		if len(o.Via) > 0 {
			payload["via"] = o.Via
		}
		out = append(out, agui.Custom(agui.CustomAttachmentWithheld, payload))
	}
	return out
}

func levelOf(a run.Attachment) string {
	if a.Sensitivity == "" {
		return "internal"
	}
	return a.Sensitivity
}

// withheldNote is what the model is told: names and levels only.
func withheldNote(withheld []run.Attachment, ceiling string) string {
	var b strings.Builder
	b.WriteString("The user attached files that were NOT given to you, because they are classified above what may be sent outside the enterprise by the model or tools this agent uses (the limit is ")
	b.WriteString(ceiling)
	b.WriteString("). You cannot read them and they are not among attachment:N. Tell the user which files were withheld and why; do not guess their content:\n")
	for _, a := range withheld {
		fmt.Fprintf(&b, "%q (%s)\n", a.Name, levelOf(a))
	}
	return strings.TrimSpace(b.String())
}
