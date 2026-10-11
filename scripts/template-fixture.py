# Reference renderings of Qwen3.8's chat template for the gateway's prompt counter
# (gateway/count.go), made with jinja2 the way transformers' apply_chat_template sets it up.
#
#   uv run --with jinja2==3.1.6 python -I scripts/template-fixture.py \
#       /path/to/chat_template.jinja gateway/testdata/qwen38-template-fixture.json
#
# Each case holds the request body as the gateway forwards it (OpenAI format: tool-call
# arguments as a JSON string) and the rendered prompt. Tools are written with sorted keys,
# as the node receives them from the gateway (Go's encoding/json sorts map keys).
#
# SPDX-License-Identifier: Apache-2.0
import hashlib
import json
import sys

import jinja2
from jinja2.ext import loopcontrols
from jinja2.sandbox import ImmutableSandboxedEnvironment

TOOLS = [
    {"type": "function", "function": {"name": "bash", "description": "Run a shell command.",
        "parameters": {"type": "object", "properties": {"cmd": {"type": "string", "description": "the command"}},
                       "required": ["cmd"]}}},
    {"type": "function", "function": {"name": "read", "description": "Read a file",
        "parameters": {"type": "object", "properties": {"path": {"type": "string"}, "limit": {"type": "integer"}}}}},
]

CASES = [
    {"messages": [{"role": "user", "content": "hi"}]},
    {"messages": [{"role": "system", "content": "  You are terse.  "}, {"role": "user", "content": "What is 2+2?"}]},
    {"messages": [{"role": "user", "content": "hi"}], "chat_template_kwargs": {"enable_thinking": False}},
    {"messages": [{"role": "user", "content": "hi"}], "reasoning_effort": "low"},
    {"messages": [{"role": "user", "content": "hi"}], "chat_template_kwargs": {"reasoning_effort": "medium"}},
    {"messages": [
        {"role": "system", "content": "Be helpful."},
        {"role": "user", "content": "Tell me a joke"},
        {"role": "assistant", "content": "Why did the GPU cross the road?", "reasoning_content": "\nA pun.\n"},
        {"role": "user", "content": [{"type": "text", "text": "Another, "}, {"type": "text", "text": "please."}]},
    ]},
    {"messages": [
        {"role": "user", "content": "first"},
        {"role": "assistant", "content": "one", "reasoning_content": "r1"},
        {"role": "user", "content": "second"},
        {"role": "assistant", "content": "two"},
        {"role": "user", "content": "third"},
    ], "chat_template_kwargs": {"preserve_thinking": False}},
    {"tools": TOOLS, "messages": [
        {"role": "system", "content": "You are an agent."},
        {"role": "user", "content": "List the files, then read README."},
        {"role": "assistant", "content": "", "tool_calls": [
            {"id": "c1", "type": "function", "function": {"name": "bash", "arguments": "{\"cmd\": \"ls -la\"}"}}]},
        {"role": "tool", "tool_call_id": "c1", "content": "README.md\nmain.go"},
        {"role": "assistant", "content": "Reading it now.", "tool_calls": [
            {"id": "c2", "type": "function", "function": {"name": "read", "arguments": "{\"path\": \"README.md\", \"limit\": 20}"}},
            {"id": "c3", "type": "function", "function": {"name": "bash", "arguments": "{\"cmd\": \"wc -l README.md\"}"}}]},
        {"role": "tool", "tool_call_id": "c2", "content": "# Title"},
        {"role": "tool", "tool_call_id": "c3", "content": "1 README.md"},
    ]},
    {"tools": TOOLS, "messages": [{"role": "user", "content": "no system message, tools only"}],
     "chat_template_kwargs": {"enable_thinking": False}},
    {"messages": [{"role": "user", "content": "Ünïcödé, 日本語, emoji 🚀 and <b>html</b> & \"quotes\""}]},
]


def sort_keys(v):
    if isinstance(v, dict):
        return {k: sort_keys(v[k]) for k in sorted(v)}
    if isinstance(v, list):
        return [sort_keys(x) for x in v]
    return v


def render(template, body):
    env = ImmutableSandboxedEnvironment(trim_blocks=True, lstrip_blocks=True, extensions=[loopcontrols])

    def raise_exception(msg):
        raise jinja2.exceptions.TemplateError(msg)

    env.filters["tojson"] = lambda x, indent=None, separators=None, sort_keys=False: json.dumps(
        x, ensure_ascii=False, indent=indent, separators=separators, sort_keys=sort_keys)
    env.globals["raise_exception"] = raise_exception
    msgs = []
    for m in body["messages"]:
        m = dict(m)
        if "tool_calls" in m:
            calls = []
            for c in m["tool_calls"]:
                c = json.loads(json.dumps(c))
                c["function"]["arguments"] = json.loads(c["function"]["arguments"])
                calls.append(c)
            m["tool_calls"] = calls
        msgs.append(m)
    kw = dict(body.get("chat_template_kwargs", {}))
    if "reasoning_effort" in body and "reasoning_effort" not in kw:
        kw["reasoning_effort"] = body["reasoning_effort"]
    tools = sort_keys(body["tools"]) if "tools" in body else None
    return env.from_string(template).render(messages=msgs, tools=tools, add_generation_prompt=True, **kw)


def main():
    src, out = sys.argv[1], sys.argv[2]
    raw = open(src, "rb").read()
    template = raw.decode("utf-8")
    lines = [json.dumps({"body": c, "prompt": render(template, c)}, ensure_ascii=False) for c in CASES]
    head = {
        "chat_template_sha256": hashlib.sha256(raw).hexdigest(),
        "source": "https://huggingface.co/Qwen/Qwen3.8-27B/resolve/1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0/chat_template.jinja",
        "generated_with": "jinja2 " + jinja2.__version__,
    }
    with open(out, "w", encoding="utf-8") as f:
        f.write(json.dumps(head, ensure_ascii=False, indent=1)[:-2] + ',\n "cases": [\n  ')
        f.write(",\n  ".join(lines) + "\n ]\n}\n")


if __name__ == "__main__":
    main()
