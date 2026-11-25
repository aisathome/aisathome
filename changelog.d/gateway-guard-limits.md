---
section: Security
---

- **The tool-call guard reads every streamed chunk, or stops the answer.** A chunk the gateway
  could not decode (for example one with a number too large for a float64, which other JSON
  decoders accept) used to go to the client unchecked, tool calls included. Every chunk is now
  decoded once, numbers as written; one that does not decode, names a key twice, or carries tool
  calls in another shape or place than a client reads breaks a stream rule (`malformed-chunk`)
  and never goes out as it came. The legacy `function_call` field is held and checked like
  `tool_calls`, and a number beside a command in tool-call arguments no longer hides it.
  Non-streaming answers go through the same checks. (gateway)
- **A node can no longer make the gateway hold large amounts of memory.** Frames from a node are
  limited by type before anything is queued: 68 KiB for a chunk (the read limit), 16 KiB for an
  error (its text cut to 2 KiB), 32 KiB for the hello and 4 KiB for anything else; a larger frame
  closes the connection. What waits in request queues is budgeted per request (1 MiB), per node
  (4 MiB) and for the gateway (64 MiB): a request whose queue would exceed one is given up on as
  one whose client is too slow. A streamed answer may total at most 1 KiB per allowed chunk (8 KiB
  with logprobs) plus 1 MiB, and never more than 128 MiB. A node announces at most 8 slots per
  model (the reference node announces 1). (gateway)
- **Anonymous node tokens are redacted from requests.** The redaction of credentials in requests
  covered account node tokens (`nt-n-…`) but not the tokens of nodes without an account
  (`nt-a-…`); it now covers every credential form the gateway issues, also inside JSON object keys
  such as the property names of a tool's schema. (gateway)
