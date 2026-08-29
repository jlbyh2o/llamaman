// Package events appends to the durable `events` table and fans each new record
// out in-process to whoever is listening, routing it to the SSE topic its
// subscribers asked for. It is the single seam between "something happened" and
// every consumer that wants to know (DESIGN section 1).
package events
