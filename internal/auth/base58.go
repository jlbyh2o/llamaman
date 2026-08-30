package auth

import "math/big"

// base58 encoding for the setup token (DESIGN §2.2a step 1: "32 crypto/rand
// bytes, base58-encode them").
//
// The alphabet is Bitcoin's: the 62 alphanumerics minus `0`, `O`, `I` and `l`.
// That is the entire reason base58 is specified here rather than base64 or hex —
// the token is READ OFF A TERMINAL AND RETYPED into a browser on another machine
// (§2.2a step 3, §13 step 10), and the four characters it omits are exactly the
// ones a human confuses. It also avoids `+`, `/` and `=`, so the token survives a
// URL, a shell argument and a copy-paste out of journald without escaping.
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58Encode encodes b, preserving leading zero bytes as leading `1`s, which
// is what makes the encoding injective — without it, two different 32-byte
// tokens could print the same.
func base58Encode(b []byte) string {
	zeros := 0
	for zeros < len(b) && b[zeros] == 0 {
		zeros++
	}

	n := new(big.Int).SetBytes(b)
	radix := big.NewInt(58)
	mod := new(big.Int)

	// The output is at most ceil(len(b) * 138 / 100) + 1 characters, which is
	// log(256)/log(58) rounded up; sizing the buffer once keeps the loop free
	// of reallocation.
	out := make([]byte, 0, len(b)*138/100+1)
	for n.Sign() > 0 {
		n.DivMod(n, radix, mod)
		out = append(out, base58Alphabet[mod.Int64()])
	}
	for i := 0; i < zeros; i++ {
		out = append(out, base58Alphabet[0])
	}

	// The digits came out least-significant first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}
