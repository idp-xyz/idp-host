package worker

import (
	"strconv"
	"time"

	"idp-host/internal/model"
	"idp-host/internal/observability"
)

// Metrics implements harness.Metrics on the Prometheus registry. Labels carry providers, models,
// gateways and outcomes only - never subjects, prompts or arguments (ADR-0001 D12).
type Metrics struct {
	steps       *observability.CounterVec
	modelSec    *observability.HistogramVec
	tokens      *observability.CounterVec
	tools       *observability.CounterVec
	toolSec     *observability.HistogramVec
	runs        *observability.CounterVec
	guards      *observability.CounterVec
	Claims      *observability.CounterVec
	memory      *observability.CounterVec
	interrupted *observability.CounterVec // D47
	authNeeded  *observability.CounterVec // D61
}

// NewMetrics registers the worker's instruments.
func NewMetrics(m *observability.Metrics) *Metrics {
	return &Metrics{
		steps:       m.Counter("idp_host_model_steps_total", "Model calls by provider, model and outcome.", "provider", "model", "outcome"),
		modelSec:    m.Histogram("idp_host_model_seconds", "Model call latency by provider.", observability.DurationBuckets, "provider"),
		tokens:      m.Counter("idp_host_model_tokens_total", "Tokens by provider, model and kind (prompt, completion).", "provider", "model", "kind"),
		tools:       m.Counter("idp_host_tool_calls_total", "Tool calls by gateway and outcome.", "gateway", "outcome"),
		toolSec:     m.Histogram("idp_host_tool_seconds", "Tool call latency by gateway.", observability.DurationBuckets, "gateway"),
		runs:        m.Counter("idp_host_runs_finished_total", "Run turns by resulting status.", "status"),
		guards:      m.Counter("idp_host_guard_events_total", "Content guard decisions by kind (input, output) and outcome.", "kind", "outcome"),
		Claims:      m.Counter("idp_host_claims_total", "Claims by result (served, fenced).", "result"),
		authNeeded:  m.Counter("idp_host_authorization_required_total", "Tool calls that answered that the member has not authorized the third-party service (D61)."),
		interrupted: m.Counter("idp_host_tool_interruptions_total", "Tool calls found cut short (D47) by whether they were sent again (resent) and why (worker_stopped, deadline, canceled).", "resent", "reason"),
		memory:      m.Counter("idp_host_memory_writes_total", "Memory records written to the CMI after completed runs, by outcome (written, replayed, rejected, failed, auth_failed, no_credential).", "outcome"),
	}
}

// StepFinished implements harness.Metrics.
func (m *Metrics) StepFinished(provider, mdl, outcome string, latency time.Duration, usage model.Usage) {
	m.steps.Inc(provider, mdl, outcome)
	m.modelSec.Observe(latency.Seconds(), provider)
	if usage.PromptTokens > 0 {
		m.tokens.Add(float64(usage.PromptTokens), provider, mdl, "prompt")
	}
	if usage.CompletionTokens > 0 {
		m.tokens.Add(float64(usage.CompletionTokens), provider, mdl, "completion")
	}
}

// ToolCall implements harness.Metrics.
func (m *Metrics) ToolCall(gateway, outcome string, latency time.Duration) {
	m.tools.Inc(gateway, outcome)
	m.toolSec.Observe(latency.Seconds(), gateway)
}

// RunFinished implements harness.Metrics.
func (m *Metrics) RunFinished(status string) { m.runs.Inc(status) }

// Guard implements harness.Metrics.
func (m *Metrics) Guard(kind, outcome string) { m.guards.Inc(kind, outcome) }

// ToolInterrupted implements the harness's interruptionMetrics (D47).
func (m *Metrics) ToolInterrupted(resent bool, reason string) {
	m.interrupted.Inc(strconv.FormatBool(resent), reason)
}

// MemoryWrite counts one remember attempt by outcome (ADR-0001 D33).
func (m *Metrics) MemoryWrite(outcome string) { m.memory.Inc(outcome) }

// AuthorizationRequired implements the harness's authorizationMetrics (D61).
func (m *Metrics) AuthorizationRequired() { m.authNeeded.Inc() }
