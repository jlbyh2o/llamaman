package fit

// The host side of section 8.1's inputs, as plain structs: per-GPU total and
// free VRAM, and free system RAM.

// Device is one participating GPU as the caller resolved it — normally from
// hw.Prober, filtered by the request's `gpus` list.
//
// FreeBytes is only meaningful when Known is true. D16 is emphatic that a probe
// failure marks a GPU unknown and NEVER zero, because a fabricated 0 MiB free
// would make this calculator confidently wrong: every verdict would be
// `wont_run`, with no sign that nothing was measured. Known carries that
// distinction across the boundary, and Estimate refuses to place a layer on a
// device it cannot measure while saying so in a note rather than in a verdict.
type Device struct {
	// Index is the device's position in the participating list, which is what
	// `tensor_split` and `main_gpu` index (section 5.7: they index the --device
	// list, not nvidia-smi's ordering).
	Index int
	UUID  string
	Name  string
	// TotalBytes and FreeBytes are BYTES. The MiB→bytes conversion happens once,
	// in internal/hw's nvidia-smi parser, and never again (section 8.6).
	TotalBytes uint64
	FreeBytes  uint64
	// Known reports whether TotalBytes and FreeBytes came from the driver.
	Known bool
}

// Host is the non-GPU half: free system RAM, and whether it was measured.
type Host struct {
	// RAMFreeBytes is `MemAvailable` from /proc/meminfo — what a new allocation
	// can actually have, not `MemFree`, which excludes reclaimable page cache and
	// would make every large model look unloadable.
	RAMFreeBytes uint64
	// RAMTotalBytes is `MemTotal`, for display.
	RAMTotalBytes uint64
	// RAMKnown distinguishes "no free RAM" from "we did not measure", for the
	// same reason Device.Known does.
	RAMKnown bool
}

// RAMHeadroom is the fraction of free system RAM section 8.7 allows a partial
// offload to spill into. The remaining tenth is the page cache and everything
// else on the host: a model sized to the last byte of MemAvailable makes the
// machine swap rather than the load fail, which is worse.
const RAMHeadroom = 0.9

// DefaultMarginMiB is `fit.margin_mib`, matching llama.cpp's own `--fit-target`
// (section 8.1). It is charged PER GPU, like OH_gpu and the request's reserve.
const DefaultMarginMiB = 1024
