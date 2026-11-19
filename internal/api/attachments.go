package api

import (
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

// hasExternalTools reports whether the release may call a tool that sends content outside the enterprise (D44; platform
// ADR-0003 D7): any tool of its effective policy (skills included) matches the externalTools patterns of its gateway, or
// the policy allows "*", because the gateway's catalogue is not known when the run is created.
func (p FilePolicy) hasExternalTools(doc *agent.Document) bool {
	tools := doc.Effective().Tools
	return tools.MatchesAny(p.ExternalTools[tools.Gateway])
}

// trigger says why the run's content is screened against the ceiling: "" when it is not (D44).
func (p FilePolicy) trigger(doc *agent.Document) string {
	var why []string
	if len(p.externalModels(doc)) > 0 {
		why = append(why, agui.ScreenExternalModel)
	}
	if p.hasExternalTools(doc) {
		why = append(why, agui.ScreenExternalTools)
	}
	return strings.Join(why, "+")
}

// screen splits the attachments of a run of doc into those it gets and those it does not. Nothing is withheld from a
// release that only uses the enterprise's own models and no external tool, or below the ceiling.
func (p FilePolicy) screen(tenant string, doc *agent.Document, atts []run.Attachment) (kept, withheld []run.Attachment, ceiling string) {
	ceiling = "internal"
	if p.Ceiling != nil {
		ceiling = p.Ceiling(tenant)
	}
	if len(atts) == 0 || p.trigger(doc) == "" {
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
func withheldEvents(withheld []run.Attachment, ceiling string, external []string, trigger string) []agui.Event {
	var out []agui.Event
	for _, a := range withheld {
		out = append(out, agui.Custom(agui.CustomAttachmentWithheld, map[string]any{"ref": a.Ref, "name": a.Name, "sensitivity": levelOf(a),
			"ceiling": ceiling, "externalModels": external, "trigger": trigger}))
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
