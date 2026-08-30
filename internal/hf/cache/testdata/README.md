# `testdata/hub` — a `huggingface_hub` cache tree, checked in

This is a real hub cache directory in `huggingface_hub`'s own layout, committed
so that the path builders in `layout.go` can be asserted against a tree nobody
generated from the same code that is under test. DESIGN section 15 asks for
exactly that for `LockPath`, and D27 explains why: the lock path is what makes
SPEC section 3.2's one-shared-cache promise true, and a path test whose
expectation is produced by the function it is testing proves nothing.

The three repositories cover the shapes that make the mapping non-trivial:

| directory | repo id | what it pins |
|---|---|---|
| `models--bartowski--Qwen3-8B-GGUF` | `bartowski/Qwen3-8B-GGUF` | the ordinary `{org}/{name}` case, plus `refs/`, a relative snapshot symlink into `blobs/`, and a `.no_exist/` negative-cache directory the scan must skip |
| `models--gpt2` | `gpt2` | a repo id with **no organization** — one `--` separator, not two |
| `models--unsloth--gemma-3-4b-it-GGUF` | `unsloth/gemma-3-4b-it-GGUF` | a name containing digits and hyphens, which must survive the mapping unescaped |

The `.locks/<repo_folder>/<etag>.lock` files are the point of the tree. Their
names are real digests: a sha256 hex for an LFS object (the blob name and the
digest are the same string, section 7.2) and a shorter non-LFS ETag for the
`gpt2` entry, so the lock path builder is exercised against both.

Blob and lock file contents are a placeholder word. Nothing here parses anything:
this tree is about **paths**. The GGUF-parsing fixtures a scan needs are built at
test time by `cachetest`, because they need synthetic GGUF bytes and real
symlinks whose targets have to resolve on the machine running the test.

The symlinks are committed as symlinks. This project targets Linux only, so a
checkout that cannot represent one is out of scope.
