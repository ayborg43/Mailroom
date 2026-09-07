// Package metrics defines the Prometheus collectors exposed at /metrics,
// registered on the default registry via promauto so any package can
// import this one and record against them without threading a registry
// reference through every constructor.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// AuthAttempts covers all three authentication surfaces (SMTP
	// submission, IMAP, webmail) under one metric family so a dashboard
	// can compare them directly. result is "success", "failure", or
	// "rate_limited".
	AuthAttempts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mailserver_auth_attempts_total",
		Help: "Authentication attempts by service and result.",
	}, []string{"service", "result"})

	// SMTPMessages counts DATA-phase outcomes. listener is "inbound" or
	// "submission".
	SMTPMessages = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mailserver_smtp_messages_total",
		Help: "SMTP messages processed by listener and result.",
	}, []string{"listener", "result"})

	// RelayAttempts counts outbound delivery attempts made by the relay
	// queue, one per (message, recipient, try).
	RelayAttempts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mailserver_relay_attempts_total",
		Help: "Outbound relay delivery attempts by result.",
	}, []string{"result"})

	// QueueDepth is a snapshot of how many messages are currently spooled
	// for outbound relay, updated on each queue sweep.
	QueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mailserver_queue_depth",
		Help: "Current number of messages in the outbound relay queue.",
	})

	// InboundAuthResults tracks SPF/DKIM/DMARC verification outcomes for
	// inbound mail. check is "spf", "dkim", or "dmarc"; result is the
	// RFC 5451 result value (pass, fail, none, softfail, temperror, ...).
	InboundAuthResults = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mailserver_inbound_auth_result_total",
		Help: "SPF/DKIM/DMARC verification results for inbound mail.",
	}, []string{"check", "result"})
)
