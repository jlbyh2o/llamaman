// Package gateway runs the per-instance public listeners: it authenticates the
// bearer token on each request, proxies to the loopback llama-server behind it,
// and accounts for the traffic. These ports are the front door for inference and
// are not the admin REST API (DESIGN sections 1 and 9).
package gateway
