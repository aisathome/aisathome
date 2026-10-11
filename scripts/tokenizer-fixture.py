# Reference token ids for the gateway's tokenizer test (gateway/testdata/).
#
# Runs Hugging Face `tokenizers` once, offline, over the model's tokenizer.json and writes the
# ids it produces for a set of tricky strings, with the file's SHA-256. The Go tokenizer
# (gateway/tokenizer.go) must reproduce them exactly.
#
#   uv run --with tokenizers==0.22.1 python -I scripts/tokenizer-fixture.py \
#       /path/to/tokenizer.json gateway/testdata/qwen38-tokenizer-fixture.json
#
# SPDX-License-Identifier: Apache-2.0
import hashlib
import json
import sys

import tokenizers

CASES = [
    "Hello, world!",
    "hi",
    "  two leading spaces and trailing   ",
    "Line one\nLine two\r\n\r\nLine four\n\n\n",
    "\tindented\twith\ttabs\n    and spaces",
    "It's, I'LL, we've, they'RE, she'd, I'm, don't, 'Ss 'ſ",
    "Numbers: 1234567890 and 3.14159, 1e-9, -42",
    "Blåbærsyltetøy og rømmegrøt på Ålesund.",
    "日本語のテキストと中文文本。",
    "Привет, мир! Ελληνικά.",
    "नमस्ते दुनिया",
    "مرحبا بالعالم",
    "emoji 👩‍💻🚀 and ✓ marks",
    # NFC text only: the gateway, like llama.cpp, skips the NFC normalizer (a decomposed
    # "e\u0301" would differ).
    "\u0301a mark first, then \u00e9 composed",
    "non\u00a0breaking\u3000ideographic space",
    "<|im_start|>user\nhi<|im_end|>\n<|im_start|>assistant\n<think>\n",
    "<tool_call>\n<function=bash>\n<parameter=cmd>\nls -la\n</parameter>\n</function>\n</tool_call>",
    "def f(x):\n    return {'a': [1, 2, 3]}  # comment\n\n\nprint(f(1))",
    "https://example.com/path?q=1&r=two#frag",
    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "!!!???...,,,;;;:::",
    "trailing newline spaces  \n  next",
    "x" + " " * 7 + "y",
    "QmFzZTY0IGVuY29kZWQgdGV4dCBsaWtlIHRoaXM=",
    "",
]


def main():
    src, out = sys.argv[1], sys.argv[2]
    raw = open(src, "rb").read()
    tok = tokenizers.Tokenizer.from_str(raw.decode("utf-8"))
    cases = []
    for text in CASES:
        ids = tok.encode(text, add_special_tokens=False).ids
        cases.append({"text": text, "ids": ids})
    doc = {
        "tokenizer_sha256": hashlib.sha256(raw).hexdigest(),
        "source": "https://huggingface.co/Qwen/Qwen3.8-27B/resolve/1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0/tokenizer.json",
        "generated_with": "tokenizers " + tokenizers.__version__,
        "cases": cases,
    }
    # One case per line, so a diff shows which string changed.
    head = {k: v for k, v in doc.items() if k != "cases"}
    lines = [json.dumps(c, ensure_ascii=False) for c in cases]
    with open(out, "w", encoding="utf-8") as f:
        f.write(json.dumps(head, ensure_ascii=False, indent=1)[:-2] + ',\n "cases": [\n  ')
        f.write(",\n  ".join(lines) + "\n ]\n}\n")


if __name__ == "__main__":
    main()
