// Package buildinfo carries the identity of the running binary. Its four
// variables are set at link time with -ldflags and are read by the version
// subcommand, the meta and healthz endpoints, and the self-update checker;
// nothing in the package has any other dependency, so every layer can import it.
package buildinfo
