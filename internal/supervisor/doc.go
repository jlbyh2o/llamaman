// Package supervisor is the reconcile loop that drives observed instance state
// toward desired state: it probes health, applies the restart policy, and
// records the fit observations that later calibrate the estimator (DESIGN
// sections 1 and 6).
package supervisor
