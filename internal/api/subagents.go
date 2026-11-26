package api

import (
	"context"
	"strings"

	"idp-host/internal/agent"
)

// maxSubagentLoads bounds the releases read to judge one run (D48). Delegation is limited to agent.MaxSubagentDepth
// levels and a release's fan-out is small, so a legitimate tree is far below it; a tree that is not is treated as
// sending content out, as an unreadable one is.
const maxSubagentLoads = 64

// viaSubagents lists why the sub-agents a run of doc can delegate to make it a run that may send content outside the
// enterprise (D48). One entry per reason, each naming the sub-agent by its path of names ("web", "research/web"):
//   - a remote A2A agent: the task text leaves the HOST;
//   - a local sub-agent whose release may use an external model, or call an external tool (judged as D43, D44 and D51
//     judge a run's own release: the operator's patterns, the gateway's marker, or a catalogue that cannot be read);
//   - a local sub-agent whose release cannot be read: unknown counts as outward, as an unknown level does in D44.
//
// It follows what D22 lets a run reach: a run at depth d delegates only while d is below its release's
// limits.maxSubagentDepth, so a sub-agent the limit makes unreachable is not counted. Each (release, depth) is read once,
// which ends a cycle, and the total number of reads is bounded; a release that would need more is counted as outward.
func (p FilePolicy) viaSubagents(ctx context.Context, tenant string, doc *agent.Document, look Catalogue) []string {
	w := walk{p: p, look: look, ctx: ctx, tenant: tenant, seen: map[walked]bool{}}
	w.visit(doc, 0, "")
	return w.via
}

type walked struct {
	digest string
	depth  int
}

type walk struct {
	p      FilePolicy
	look   Catalogue
	ctx    context.Context
	tenant string
	seen   map[walked]bool
	loads  int
	via    []string
}

func (w *walk) visit(doc *agent.Document, depth int, path string) {
	limit := doc.Definition.Limits.MaxSubagentDepth
	if limit == 0 {
		limit = agent.DefaultLimits.MaxSubagentDepth
	}
	if depth >= limit || len(doc.Subagents) == 0 {
		return // D22: this run cannot delegate
	}
	for _, sub := range doc.Subagents {
		name := path + sub.Name
		if sub.A2A != nil {
			w.via = append(w.via, "remote agent "+name+": the task text leaves the HOST")
			continue
		}
		if depth >= agent.MaxSubagentDepth || w.loads >= maxSubagentLoads {
			w.via = append(w.via, "sub-agent "+name+": too many levels or releases to judge")
			continue
		}
		key := walked{sub.Digest, depth + 1}
		if w.seen[key] {
			continue
		}
		w.seen[key] = true
		w.loads++
		var child *agent.Document
		var err error
		if w.p.Release != nil {
			child, err = w.p.Release(w.ctx, w.tenant, sub.AgentID, sub.Digest)
		}
		if w.p.Release == nil || err != nil || child == nil {
			w.via = append(w.via, "sub-agent "+name+": its release cannot be read, so it counts as sending content out")
			continue
		}
		var models []string
		for _, m := range w.p.externalModels(child) {
			if !strings.HasPrefix(m, "a2a:") { // its remote agents are named when the walk gets to them
				models = append(models, m)
			}
		}
		if len(models) > 0 {
			w.via = append(w.via, "sub-agent "+name+": may use an external model ("+strings.Join(models, ", ")+")")
		}
		switch v := w.p.judgeTools(child, w.look); {
		case v.unknown != "":
			w.via = append(w.via, "sub-agent "+name+": may call an external tool (the catalogue of gateway "+v.unknown+" could not be read, so its tools count as sending content out)")
		case len(v.marked) > 0:
			w.via = append(w.via, "sub-agent "+name+": may call an external tool (marked by the gateway: "+namedTools(v.marked)+")")
		case v.outward:
			w.via = append(w.via, "sub-agent "+name+": may call an external tool")
		}
		w.visit(child, depth+1, name+"/")
	}
}

// subagentsHaveKnowledgeTools reports whether a local sub-agent a run of doc can reach has the knowledge tools on (D65, D68): such a
// run keeps the member's CMI credential and numbers its first package, so that the sub-agent can use the tools as the member.
// It follows what D22 lets a run reach, reads each (release, depth) once and at most maxSubagentLoads releases; a release that
// cannot be read is not counted (nothing is lost: the sub-agent then has no credential and no tools).
func (p FilePolicy) subagentsHaveKnowledgeTools(ctx context.Context, tenant string, doc *agent.Document) bool {
	if p.Release == nil {
		return false
	}
	seen, loads := map[walked]bool{}, 0
	var visit func(d *agent.Document, depth int) bool
	visit = func(d *agent.Document, depth int) bool {
		limit := d.Definition.Limits.MaxSubagentDepth
		if limit == 0 {
			limit = agent.DefaultLimits.MaxSubagentDepth
		}
		if depth >= limit || depth >= agent.MaxSubagentDepth {
			return false
		}
		for _, sub := range d.Subagents {
			key := walked{sub.Digest, depth + 1}
			if sub.A2A != nil || seen[key] || loads >= maxSubagentLoads {
				continue
			}
			seen[key] = true
			loads++
			child, err := p.Release(ctx, tenant, sub.AgentID, sub.Digest)
			if err != nil || child == nil {
				continue
			}
			if child.Definition.Context.HasKnowledgeTools() || visit(child, depth+1) {
				return true
			}
		}
		return false
	}
	return visit(doc, 0)
}
