// Package metrics holds process-wide counters, exported for Prometheus on the
// admin API (/metrics).
package metrics

import (
	"fmt"
	"io"
	"sort"
	"sync/atomic"
)

// Metrics is a small set of atomic counters.
type Metrics struct {
	AuthSuccess       atomic.Int64
	AuthFailure       atomic.Int64
	MessagesQueued    atomic.Int64
	DeliveryAttempts  atomic.Int64
	MessagesDelivered atomic.Int64
	MessagesDead      atomic.Int64
	Requeued          atomic.Int64
}

// WritePrometheus renders the counters in the Prometheus text format.
func (m *Metrics) WritePrometheus(w io.Writer) {
	type counter struct {
		name  string
		help  string
		value func() int64
	}
	counters := []counter{
		{"carteiro_auth_success_total", "Successful SMTP AUTH logins.", m.AuthSuccess.Load},
		{"carteiro_auth_failure_total", "Failed SMTP AUTH attempts.", m.AuthFailure.Load},
		{"carteiro_messages_queued_total", "Messages accepted and queued.", m.MessagesQueued.Load},
		{"carteiro_delivery_attempts_total", "Delivery attempts towards MX servers.", m.DeliveryAttempts.Load},
		{"carteiro_messages_delivered_total", "Messages fully delivered.", m.MessagesDelivered.Load},
		{"carteiro_messages_dead_total", "Messages moved to dead-letter.", m.MessagesDead.Load},
		{"carteiro_messages_requeued_total", "Dead messages requeued through the API.", m.Requeued.Load},
	}
	sort.Slice(counters, func(i, j int) bool { return counters[i].name < counters[j].name })
	for _, c := range counters {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", c.name, c.help, c.name, c.name, c.value())
	}
}
