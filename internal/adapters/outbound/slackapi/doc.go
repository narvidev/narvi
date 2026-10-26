// Package slackapi holds the single production Slack Web API client
// (§30.3's own "one client per provider" compensating control) -- a real
// HTTP client against chat.postMessage/chat.postEphemeral/chat.update/
// views.open/users.info/auth.test, implementing ports.Notifier (called by the
// outbox delivery worker, internal/app/outboxworker) AND providing the
// narrow surface internal/app/shadowslack's Decorator wraps for
// internal/adapters/inbound/slack's own synchronous ack/interactivity
// writes.
//
// This USED TO be two small, independently-constructed clients: this
// package's own Client (outbox-only) and internal/adapters/inbound/
// slack/ack.go's own private ackClient (in-thread acks, constructed
// straight from the ingress package via newAckClient). §30.3 is explicit
// that the single-instance property GitHub's transport gate gets for
// free does NOT hold for Slack unless something enforces it: two
// independently-constructed clients meant two independently-gate-able (or
// gate-FREE) egress paths. ack.go's own client is retired; every Slack
// write in this codebase, synchronous or outboxed, now goes through THIS
// package's Client, and internal/adapters/inbound/slack no longer
// constructs a client of its own at all -- it is handed the shadowslack-
// decorated seam by production wiring instead (cmd/control-plane/main.go).
package slackapi
