// Package openapi generates api/openapi.json from the route registry and
// provides the response-conformance checker that integration tests run: an
// undocumented endpoint, a missing documented field or an extra field fails the
// suite, so the committed spec can never drift from the code (DESIGN section 3,
// D43).
package openapi
