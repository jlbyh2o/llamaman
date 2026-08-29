// Package api holds the REST surface: the stdlib http.ServeMux route registry
// using Go 1.22+ method-and-pattern routes, the handlers, the DTOs that convert
// storage forms to wire forms, and the error envelope defined in DESIGN section
// 3. No domain package imports it — dependencies point inward (DESIGN section 1,
// invariant 4).
package api
