#!/usr/bin/env python3
"""Generate testdata/fit/corpus.json from DESIGN sections 8.2-8.4.

This is a SECOND implementation of the fit formulas, written from the design
text rather than from internal/fit, so the Go calculator is checked against the
specification and not against itself.  It is kept beside the corpus it produced
so a row can be re-derived and a disagreement can be attributed.
"""

import json
from fractions import Fraction

MIB = 1 << 20
GIB = 1 << 30

# ggml block sizes and block byte sizes (internal/gguf/types.go, and upstream's
# sizeof(block_*)).
TYPES = {
    "f32": (1, 4),
    "f16": (1, 2),
    "q8_0": (32, 34),
    "q4_K": (256, 144),
    "q6_K": (256, 210),
}

# Cache-type bytes per element (DESIGN section 8.3's table).
BPE = {"f16": Fraction(2), "q8_0": Fraction(34, 32)}


def tbytes(typ, dims):
    block, size = TYPES[typ]
    numel = 1
    for d in dims:
        numel *= d
    return numel // block * size


def dense_layers(n, embd, ff, typ):
    out = []
    for i in range(n):
        p = f"blk.{i}."
        out.append((p + "attn_norm.weight", "f32", (embd,)))
        out.append((p + "attn_qkv.weight", typ, (embd, embd * 3)))
        out.append((p + "ffn_up.weight", typ, (embd, ff)))
        out.append((p + "ffn_down.weight", typ, (ff, embd)))
    return out


def llama():
    t = [("token_embd.weight", "q4_K", (512, 256))]
    t += dense_layers(4, 512, 1024, "q4_K")
    t += [("output_norm.weight", "f32", (512,)),
          ("output.weight", "q6_K", (512, 256))]
    return dict(arch="llama", n_layer=4, n_embd=512, n_ff=1024, n_head=8,
                n_head_kv=[2] * 4, head_dim_k=64, head_dim_v=64, n_vocab=256,
                n_expert=0, n_expert_used=0, swa_window=None, swa_pattern=None,
                tensors=t)


def qwen3():
    t = [("token_embd.weight", "q8_0", (1024, 512))]
    t += dense_layers(6, 1024, 2560, "q8_0")
    t += [("output_norm.weight", "f32", (1024,))]
    return dict(arch="qwen3", n_layer=6, n_embd=1024, n_ff=2560, n_head=16,
                n_head_kv=[8] * 6, head_dim_k=128, head_dim_v=128, n_vocab=151936,
                n_expert=0, n_expert_used=0, swa_window=None, swa_pattern=None,
                tensors=t)


def gemma3():
    t = [("token_embd.weight", "q4_K", (768, 64))]
    t += dense_layers(6, 768, 1536, "q4_K")
    t += [("output_norm.weight", "f32", (768,))]
    return dict(arch="gemma3", n_layer=6, n_embd=768, n_ff=1536, n_head=8,
                n_head_kv=[4, 4, 4, 2, 2, 1], head_dim_k=256, head_dim_v=256,
                n_vocab=64, n_expert=0, n_expert_used=0,
                swa_window=1024, swa_pattern=6, tensors=t)


def moe():
    embd, ff, nexp, layers = 512, 768, 8, 4
    t = [("token_embd.weight", "q4_K", (embd, 256))]
    for i in range(layers):
        p = f"blk.{i}."
        t.append((p + "attn_norm.weight", "f32", (embd,)))
        t.append((p + "attn_qkv.weight", "q4_K", (embd, embd * 3)))
        t.append((p + "ffn_gate_inp.weight", "f32", (embd, nexp)))
        t.append((p + "ffn_up_exps.weight", "q4_K", (embd, ff, nexp)))
        t.append((p + "ffn_down_exps.weight", "q4_K", (ff, embd, nexp)))
    t += [("output_norm.weight", "f32", (embd,)),
          ("output.weight", "q6_K", (embd, 256))]
    return dict(arch="qwen3moe", n_layer=layers, n_embd=embd, n_ff=ff, n_head=8,
                n_head_kv=[2] * layers, head_dim_k=64, head_dim_v=64,
                n_vocab=151936, n_expert=nexp, n_expert_used=2,
                swa_window=None, swa_pattern=None, tensors=t)


FIXTURES = {"llama": llama(), "qwen3": qwen3(), "gemma3": gemma3(), "moe": moe()}


def weights(m):
    return sum(tbytes(typ, dims) for _, typ, dims in m["tensors"])


def is_swa(m, i):
    w, p = m["swa_window"], m["swa_pattern"]
    if not w or not p or p <= 1:
        return False
    return ((i + 1) % p) != 0


def kv(m, n_ctx, type_k, type_v, n_ubatch):
    kv_ctx = -(-n_ctx // 256) * 256
    per_tok = [
        m["n_head_kv"][i] * (Fraction(m["head_dim_k"]) * BPE[type_k]
                             + Fraction(m["head_dim_v"]) * BPE[type_v])
        for i in range(m["n_layer"])
    ]
    full = sum(per_tok[i] for i in range(m["n_layer"]) if not is_swa(m, i))
    swa = sum(per_tok[i] for i in range(m["n_layer"]) if is_swa(m, i))
    kv_full = int(kv_ctx * full)
    kv_swa = int(min(kv_ctx, m["swa_window"] + n_ubatch) * swa) if swa else 0
    return kv_full, kv_swa, kv_ctx


def compute(m, n_ubatch, n_parallel, n_ctx, flash_attn, kv_ctx):
    logits = m["n_vocab"] * max(n_ubatch, n_parallel) * 4
    act = 6 * n_ubatch * m["n_embd"] * 4
    if flash_attn:
        attn = 2 * n_ubatch * m["n_head"] * m["head_dim_k"] * 4
    else:
        attn = m["n_head"] * n_ubatch * min(kv_ctx, 4096) * 4
    moe_term = m["n_expert_used"] * n_ubatch * m["n_ff"] * 4 if m["n_expert"] else 0
    return logits + act + attn + moe_term


OVERHEAD = 400 * MIB
MARGIN = 1024 * MIB

CASES = [
    ("llama", 4096, "f16", "f16", True),
    ("llama", 4096, "f16", "f16", False),
    ("llama", 2048, "f16", "f16", True),
    ("llama", 8192, "f16", "f16", True),
    ("llama", 4096, "q8_0", "q8_0", True),
    ("llama", 16384, "q8_0", "q8_0", True),
    ("qwen3", 4096, "f16", "f16", True),
    ("qwen3", 8192, "f16", "f16", True),
    ("qwen3", 32768, "f16", "f16", True),
    ("qwen3", 32768, "q8_0", "q8_0", True),
    ("qwen3", 8192, "f16", "f16", False),
    ("gemma3", 2048, "f16", "f16", True),
    ("gemma3", 8192, "f16", "f16", True),
    ("gemma3", 8192, "q8_0", "q8_0", True),
    ("gemma3", 4096, "f16", "f16", False),
    ("moe", 4096, "f16", "f16", True),
    ("moe", 8192, "f16", "f16", True),
    ("moe", 8192, "q8_0", "q8_0", True),
    ("moe", 2048, "f16", "f16", False),
    ("qwen3", 2048, "q8_0", "q8_0", False),
]


def main():
    rows = []
    for name, n_ctx, tk, tv, fa in CASES:
        m = FIXTURES[name]
        w = weights(m)
        kv_full, kv_swa, kv_ctx = kv(m, n_ctx, tk, tv, 512)
        cb = compute(m, 512, 1, n_ctx, fa, kv_ctx)
        assigned = w + kv_full + kv_swa + cb + OVERHEAD + MARGIN

        # Each configuration is placed on a card that either comfortably holds
        # it or is genuinely too small for it.  An OOM row's free VRAM is short
        # of the REAL allocation -- weights, cache, compute buffers and the
        # backend context, with no margin -- because that is what an OOM is: the
        # margin is our headroom, not llama.cpp's, and a row that only exceeded
        # the margin would make section 8.7's non-negotiable rule trivial.
        oom = (len(rows) % 3 == 2)
        real = w + kv_full + kv_swa + cb + OVERHEAD
        if oom:
            free = real - 32 * MIB
        else:
            free = assigned + 512 * MIB
        rows.append({
            "name": f"{name} {tk}/{tv} c={n_ctx} fa={'on' if fa else 'off'}"
                    f"{' (OOM)' if oom else ''}",
            "fixture": name,
            "n_ctx": n_ctx,
            "n_ubatch": 512,
            "n_batch": 2048,
            "n_parallel": 1,
            "type_k": tk,
            "type_v": tv,
            "flash_attn": "on" if fa else "off",
            "free_vram_bytes": free,
            "ram_free_bytes": 32 * GIB,
            "observed": {
                "weights_bytes": w,
                "kv_bytes": kv_full,
                "kv_swa_bytes": kv_swa,
                "compute_bytes": cb,
            },
            "oom": oom,
        })

    doc = {
        "note": (
            "Section 8.7's golden corpus. Each row is one load of one of the "
            "four synthetic GGUF fixtures in internal/fit/golden_test.go, with "
            "the three buffer figures llama.cpp prints at startup. They are "
            "derived by gencorpus.py, a SECOND implementation of sections "
            "8.2-8.4 written from the design text, so the Go calculator is "
            "compared against the specification rather than against itself. "
            "Rows recorded from a real host -- a `fit_observations` row's "
            "actual_weights_bytes / actual_kv_bytes / actual_compute_bytes and "
            "its `oom` flag -- have the same shape and drop straight in beside "
            "these; the compute-buffer figure is the one that most needs them, "
            "because section 8.7 calls it the only genuinely empirical term."
        ),
        "loads": rows,
    }
    print(json.dumps(doc, indent=2))


if __name__ == "__main__":
    main()
