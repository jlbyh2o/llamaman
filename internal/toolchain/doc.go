// Package toolchain probes the host for the tools a source build needs — gcc,
// g++, cmake, ninja, git, make, nvcc — plus the NVIDIA driver and the glibc
// version, and reports each result in the form the setup wizard and the system
// screen render as a card with fix guidance (DESIGN section 1).
package toolchain
