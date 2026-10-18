# Credit

Volunteers earn credit for the GPU time their nodes give others. It is recognition in
BOINC's unit, never money, and it is anchored to BOINC's
cobblestone for decode: one output token of a 27B model is about 2 × 27e9 FLOPs, and
4.32e11 FLOPs make one credit.

| Per token | Credit |
|---|---:|
| output (generated: text, reasoning, tool calls, end of turn) | 0.125 |
| new prompt token | 0.0125 |
| cached prompt token | 0.00125 |

- **Counted by the gateway.** It renders the forwarded request with Qwen3.8's chat template
  (`gateway/count.go`) and counts it with its own byte-level BPE tokenizer
  (`gateway/tokenizer.go`); the output from the text and tool calls it relayed. Node-reported
  counts are kept in the usage log (`usage`) but not used: each line has `counted`
  (`"gateway"`, or `"node"` without a tokenizer, when the node's counts are used, clamped as
  for budgets), `tokens` and `credit`.
- **As the engine renders it.** Prompts are counted as the installed llama-server renders
  them (`--no-reasoning-preserve`): earlier turns' reasoning counts only when the request sets
  `chat_template_kwargs.preserve_thinking: true` (or llama.cpp's `preserve_reasoning`).
- **Cache decided by the gateway.** A prompt counts as cached up to the longest prefix the
  same node served the same requester (account, chat user or key) within 10 minutes. Only
  chained hashes of 256-byte blocks are kept, bounded per node (32 conversations of up to
  128k tokens) and in all (256 nodes). The node's cache claim is ignored.
- **No credit if no value is delivered** (owner, 2026-10-18). Credit is for what reached the
  client:
  - `ok` (the client got the whole answer, end included): the prompt, all output and the
    end-of-turn token.
  - `cancelled` (the client left, was too slow, or a write to it failed): the prompt and the
    output that was written to the client, as the node sent it (`tokens.completion` counts
    only that part; no end-of-turn token, and no end of thinking if the answer broke off in
    its reasoning). Text the secret restorer still holds back (it could be the start of a
    placeholder) has not been written, and counts as the node wrote it once it is, never as
    the restored secret. An answer whose end (held tool calls, `[DONE]`) never reached the
    client is cancelled too, and its undelivered tool calls earn nothing; so is one whose
    client's request ended while the end went out, even if the writes still succeeded (a
    closed connection takes them for a while).
  - **Nothing at all**, not even for the prompt, when no generated text reached the client:
    a cancel before the first relayed token, or a non-streamed answer whose write failed or
    whose client left before it was sent.
  - Without a tokenizer (`counted: "node"`) a cancelled answer's output is the node's count,
    at most one token per chunk written to the client.
- **No credit** for self-served answers (an account's own node), failed or blocked answers.
  They are metered in full (`tokens`) for the usage log and statistics.
- **Ledger** per account, per node and per config-token operator (the maintainer's nodes,
  kept apart from accounts): today (UTC), total and BOINC's `expavg_credit`
  (`update_average` from `lib/util.cpp`, 7-day half-life; the first grant is spread over the
  time since the account or node was created, at least a day).
- **Usernames.** An account may set a username on its account page (3–32 letters, digits,
  spaces, `- _ . '`; unique ignoring case; no web addresses). Setting it is the consent to
  appear publicly; without one an account counts in totals only. `aisathome-gateway accounts
  username-clear <id>` removes an offensive one; `accounts list` shows them.
- **Public statistics** (no authentication): `GET /v1/stats/credit` (leaderboard: top 100
  named volunteers, anonymous and maintainer totals; rebuilt at most every minute), and
  `/stats/tables.xml`, `user.gz`, `host.gz`, `team.gz` in BOINC's `db_dump` format, named
  accounts only, rebuilt at most hourly, with an ETag (Gridcoin's scraper needs one). Node
  ids, labels, account ids and pseudonyms never appear; hosts have numeric ids. No CPIDs yet.
  The reverse proxy must route `/stats/*` to the gateway (like `/account*` and `/v1/*`).

**The tokenizer file.** Qwen3.8's `tokenizer.json` (12.8 MB, not in the repo):

```bash
curl -fLo tokenizer.json \
  https://huggingface.co/Qwen/Qwen3.8-27B/resolve/1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0/tokenizer.json
sha256sum tokenizer.json   # 0997f410c57a1f4e53b09e4be8f4a172d90edd9564368fb0847030937229b9f3
```

Set `"tokenizer_file"` to its absolute path and `"tokenizer_sha256"` to that hash. It uses
about 30 MB of memory and counts roughly 2 million tokens a second. Only the Qwen3.5+
pre-tokenizer is supported (the gateway checks the regex when it loads the file). Like
llama.cpp, the gateway skips the file's NFC normalizer, so text that is not in NFC can count
a token or two differently than Hugging Face does. The tests check the tokenizer against
Hugging Face `tokenizers` (`scripts/tokenizer-fixture.py`) and the template against jinja2
(`scripts/template-fixture.py`); CI uses a cut-down copy of the file
(`gateway/testdata/qwen38-mini-tokenizer.json`), and `AISATHOME_TOKENIZER=/path/to/tokenizer.json
go test ./gateway -run TestTokenizerFullFile` checks the full one.
