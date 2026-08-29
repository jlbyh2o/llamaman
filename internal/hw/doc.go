// Package hw reports what hardware this host has: GPUs behind the Prober
// interface (whose one v1 implementation shells out to nvidia-smi, per D16),
// system memory from /proc/meminfo, and free disk space via Statfs. A probe
// failure marks GPUs unknown, never zero, so a missing driver cannot be read as
// "no VRAM" (DESIGN sections 1 and 8.6).
package hw
