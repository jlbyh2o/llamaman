// Package tokens mints, hashes and verifies the API tokens the gateway accepts,
// and keeps a cache of verified tokens behind an epoch counter so revoking or
// editing one invalidates every cached decision at once (DESIGN sections 1 and
// 9).
package tokens
