// Package cache owns the Hugging Face hub cache layout on disk: the path
// scheme, the lock files that keep two writers apart, the blobs and the
// snapshot symlinks that point at them, scanning an existing cache to
// reconstruct what is there, and deleting an entry safely (DESIGN section 1).
package cache
