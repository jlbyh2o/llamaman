// Package secrets seals and opens the values that must not sit in the database
// in plaintext — the Hugging Face token above all — using an AES-GCM box keyed
// by a 0600 key file beside the database (DESIGN section 1).
package secrets
