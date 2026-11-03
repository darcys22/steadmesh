# Structured task results

A disposable task can request a machine-readable result without changing its
harness, model, credentials or tools. Set `output_schema` on `tasks.start`, the
runtime task API, or a `teams.expand` worker's task. Omit it for existing text
behaviour.

For example, a review task can require:

```json
{
  "type": "object",
  "properties": {
    "accepted": {"type": "boolean"},
    "findings": {"type": "array", "maxItems": 20, "items": {"type": "string"}}
  },
  "required": ["accepted", "findings"],
  "additionalProperties": false
}
```

The final answer must be exactly one JSON value. Steadmesh does not extract JSON
from prose or Markdown fences, coerce strings to numbers, or truncate JSON to
make it fit. A successful task returns `structured_output` and the same JSON in
`output`. `tasks.get` includes the structured value. Schema-valid `accepted:
false` is a valid technical result, not a transport failure or reason to try
another agent until one agrees.

## Supported schema and limits

The initial contract is an explicit subset of JSON Schema, not arbitrary JSON
Schema. Every schema node must be an object with one concrete `type`: object,
array, string, number, integer, boolean or null. Supported keywords are:

- `type`, `title`, `description`
- `properties`, `required`, boolean `additionalProperties`
- one schema in `items`, `minItems`, `maxItems`
- `minLength`, `maxLength`, `minimum`, `maximum`
- `enum` containing 1–64 scalar values

Unknown keywords fail admission. References, recursive schemas, unions,
combinators, regular expressions, format checks, defaults and external schema
loading are not supported. No schema may load a URL or local file.

Schemas are limited to 16 KiB and 256 schema nodes. JSON nesting is limited to 16
levels; output is limited to 16 KiB. Duplicate object keys and invalid UTF-8 are
rejected. Numeric constraints use binary64 semantics; numbers outside the exact
integer range ±9,007,199,254,740,991 are rejected. Represent large identifiers or
exact decimal amounts as strings. Numeric tokens are limited to 128 bytes and
exponents to −308 through 308; values that underflow or lose their
integer/fractional distinction are rejected.

## Harness capability and trust boundaries

Claude Code, Codex, Pi and the fake harness advertise `platform_validated` output.
The shared delivery formatter supplies the requested shape as task data, and the
platform validates the returned text. This does **not** mean the harness natively
constrains token generation. Future adapters may advertise `native`, but their
results must pass the same validator. Adapters without either capability fail
before delivery rather than silently ignoring the request.

A schema mismatch ends the task with `structured_output_invalid`. It is distinct
from a transport failure. This path never retries a potentially side-effectful
turn. The platform repeats validation when accepting the worker's result and
rejects missing, unsolicited or inconsistent structured results. Schema-valid
content remains untrusted agent output: it is not business approval, permission
to apply artifacts, or evidence that an external action succeeded.
