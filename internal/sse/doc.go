// Package sse is the Server-Sent Events transport: subscription management,
// heartbeats that keep intermediaries from closing an idle stream, and
// backpressure handling that drops frames for a slow client rather than
// stalling the producer. It carries frames; it does not decide what they mean
// (DESIGN section 1).
package sse
