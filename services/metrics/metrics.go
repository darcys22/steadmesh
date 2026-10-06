// Package metrics defines the platform's Prometheus metrics (§14). Labels
// never carry credentials or message content.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics groups the platform's collectors.
type Metrics struct {
	// IngressEvents counts inbound human events by result: accepted,
	// duplicate, unknown_user, over_cap or error.
	IngressEvents *prometheus.CounterVec
	// MessagesAccepted counts durably accepted messages by origin.
	MessagesAccepted *prometheus.CounterVec
	// MessagesRejected counts refused internal sends by reason.
	MessagesRejected *prometheus.CounterVec
	QueueAge         *prometheus.GaugeVec
	Seats            *prometheus.GaugeVec
	DeadLettered     prometheus.Counter
	MemoryConflicts  prometheus.Counter
	ConnectorRetries *prometheus.CounterVec
	ConnectorOps     *prometheus.CounterVec
	ConnectorUnknown *prometheus.CounterVec
	ToolCalls        *prometheus.CounterVec
	HTTPRequests     *prometheus.CounterVec
	HTTPDuration     *prometheus.HistogramVec
	SchedulesFired   prometheus.Counter
}

// New creates the metrics and registers them with reg.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		IngressEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "steadmesh_ingress_events_total", Help: "Inbound channel events by result.",
		}, []string{"connection", "result"}),
		MessagesAccepted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "steadmesh_messages_accepted_total", Help: "Durably accepted messages by origin.",
		}, []string{"origin"}),
		MessagesRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "steadmesh_messages_rejected_total", Help: "Rejected internal messages by reason.",
		}, []string{"reason"}),
		QueueAge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "steadmesh_inbox_oldest_pending_seconds", Help: "Age of the oldest undelivered message per organisation.",
		}, []string{"organization"}),
		Seats: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "steadmesh_seats", Help: "Active seats by execution state (Blocked seats included).",
		}, []string{"organization", "state"}),
		DeadLettered: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "steadmesh_deliveries_dead_lettered_total", Help: "Deliveries dead-lettered after exhausting attempts.",
		}),
		MemoryConflicts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "steadmesh_memory_conflicts_total", Help: "Memory revisions rejected for a stale expected revision.",
		}),
		ConnectorRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "steadmesh_connector_retries_total", Help: "Connector calls retried after a retryable failure.",
		}, []string{"connection"}),
		ConnectorOps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "steadmesh_connector_operations_total", Help: "Connector operations by final status.",
		}, []string{"connection", "status"}),
		ConnectorUnknown: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "steadmesh_connector_unknown_outcomes_total", Help: "Connector operations recorded with an unknown outcome.",
		}, []string{"connection"}),
		ToolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "steadmesh_tool_calls_total", Help: "Tool calls by tool and result.",
		}, []string{"tool", "result"}),
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "steadmesh_http_requests_total", Help: "HTTP requests by route pattern and status code.",
		}, []string{"route", "code"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "steadmesh_http_request_duration_seconds", Help: "HTTP request latency by route pattern.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route"}),
		SchedulesFired: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "steadmesh_wake_schedules_fired_total", Help: "Scheduled wakes queued.",
		}),
	}
	reg.MustRegister(m.IngressEvents, m.MessagesAccepted, m.MessagesRejected, m.QueueAge, m.Seats, m.DeadLettered,
		m.MemoryConflicts, m.ConnectorRetries, m.ConnectorOps, m.ConnectorUnknown, m.ToolCalls, m.HTTPRequests,
		m.HTTPDuration, m.SchedulesFired)
	return m
}

// NewUnregistered returns metrics registered with a private registry, for tests.
func NewUnregistered() *Metrics { return New(prometheus.NewRegistry()) }
