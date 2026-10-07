# Contract: Claude Code 2.1.289

## Pinned version and how it was obtained

- Version: **2.1.289**. It exists on npm, and `claude --version` prints `2.1.289 (Claude Code)` (contract/claude-version.txt).
- `npm pack --ignore-scripts` was used for every package. No install scripts were run. The wrapper's `postinstall` (`node install.cjs`) was not executed.

| Package | Tarball sha256 |
|---|---|
| `@anthropic-ai/claude-code@2.1.289` (wrapper, no JS bundle, `bin/claude.exe` placeholder plus optionalDependencies) | `d72b0a94b84e51fbe5686577f322488a89f844c275985e8054e16951c6dd3e64` |
| `@anthropic-ai/claude-code-darwin-arm64@2.1.289` | `0aa86cf337ede6041513a72e814e703a5d94c1e75e983f0e5b0e3f2ed1e0b700` |
| `@anthropic-ai/claude-code-linux-x64@2.1.289` (pod target) | `50a6eb433422febcfa7b7ac1e78ba795cdf79f1fe58f017fb1d9aeb9491b337d` |

Binary sha256 values:
- darwin-arm64 `package/claude`: `03d66745e3bb69ec727d66023696f3820bc0a00a8a5ba725eb6706d0c67cbe69`
- linux-x64 `package/claude`: `a186b99e4a9c88366cd49df2f7dad56c61fc306ef0140b19ee64b7c42a8d1348`

The full list is in contract/claude-tarballs.sha256.

**Important packaging fact:** since 2.x there is **no `cli.js`**. The npm wrapper picks a per-platform Bun single-file executable from optionalDependencies. The minified JS is embedded readably in the binary, so the evidence below is the byte offset in the darwin-arm64 binary, found by `ctx.py`, a text search of the binary that was never executed. The linux-x64 binary has the same counts for every key string (contract/claude-grep-counts.txt). For the pod, pin `@anthropic-ai/claude-code-linux-x64@2.1.289` (or the `-musl` variant for Alpine) directly, or install the wrapper with scripts enabled in the image build.

`--help` was run with `env -i HOME=<scratch>` and `DISABLE_AUTOUPDATER=1`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`. The output is in contract/claude-help.txt.

## CLI flags (all VERIFIED in contract/claude-help.txt)

| Flag | Verified text |
|---|---|
| `-p, --print` | "Print response and exit … The workspace trust dialog is skipped when Claude is run in non-interactive mode … Settings files that fail validation are silently ignored in this mode" |
| `--output-format` | choices `text`, `json`, `stream-json` (only with --print) |
| `--input-format` | choices `text`, `stream-json` (only with --print). This is needed for stdin control requests. |
| `--verbose` | "Override verbose mode setting from config". Required with `-p --output-format stream-json`; that requirement comes from earlier versions and was not re-tested here (**UNVERIFIED** for 2.1.289). |
| `--bare` | Skips hooks, LSP, plugin sync, attribution, auto-memory, background prefetches, keychain reads, and CLAUDE.md auto-discovery. Sets `CLAUDE_CODE_SIMPLE=1`. "Anthropic auth is strictly ANTHROPIC_API_KEY or apiKeyHelper via --settings (OAuth and keychain are never read)." Context must be supplied explicitly via --system-prompt / --append-system-prompt / --add-dir / --mcp-config / --settings / --agents / --plugin-dir. |
| `-r, --resume [value]` | "Resume a conversation by session ID". There is also `--session-id <uuid>`, `--fork-session`, `-c/--continue` and `--no-session-persistence`. |
| `--mcp-config <configs...>` | "Load MCP servers from JSON files or strings (space-separated)" |
| `--strict-mcp-config` | "Only use MCP servers from --mcp-config, ignoring all other MCP configurations" |
| `--settings <file-or-json>` | "Path to a settings JSON file or a JSON string". There is also `--setting-sources user,project,local`. |
| `--permission-mode <mode>` | choices **`acceptEdits`, `auto`, `bypassPermissions`, `manual`, `dontAsk`, `plan`**. Internally `manual` is normalised to `default`, and `default` is also accepted. Evidence at offset 179713448: `b$=["acceptEdits","auto","bypassPermissions","default","dontAsk","plan"] … function Rg(e){return e==="manual"?"default":e}` |
| `--permission-prompts <target>` | **PRESENT**. choices `host` (default) and `none`. With `none`: "nobody: anything that would prompt is denied automatically; the permission mode still decides everything else". |
| `--append-system-prompt <prompt>` | present. There is also `--system-prompt` and `--system-prompt-snapshot on|off`. The default `on` records the system prompt on the first request and reuses it on resume, even if a later launch passes different text. |
| `--model <model>` | alias or full model name. There is also `--fallback-model` and `--effort`. |

Other flags of note: `--tools`, `--allowed-tools`, `--disallowed-tools`, `--restricted`, `--include-partial-messages`, `--replay-user-messages`, `--max-budget-usd`, `--dangerously-skip-permissions`.

## Environment and auth

- **`CLAUDE_CODE_API_KEY_HELPER_TTL_MS`**: VERIFIED. Offset 182320945:
  ```
  function lls(){let e=a.CLAUDE_CODE_API_KEY_HELPER_TTL_MS;if(e!==void 0){if(e>=0)return e;t(`Found CLAUDE_CODE_API_KEY_HELPER_TTL_MS env var, but it was not a valid number. Got ${e}`…)}return KH}
  ```
  The default is `var KH=300000,kk=30000` at offset 182306865, so the helper result is cached for **5 min** by default. `kk` (30 s) appears to be an expiry skew. The exact refresh-ahead semantics are **UNVERIFIED**.
- **`ANTHROPIC_BASE_URL`**: VERIFIED as honoured. Offset 180333754:
  ```
  function ag(){let e=process.env.ANTHROPIC_BASE_URL;if(!e)return!0;return my(e)} … function my(e){… return["api.anthropic.com"].includes(t)}
  ```
  A non-anthropic host marks the session as not first-party, which disables first-party-only features.
- `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_CUSTOM_HEADERS`, `CLAUDE_CONFIG_DIR`, `DISABLE_AUTOUPDATER`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, `DISABLE_TELEMETRY`, `MCP_TIMEOUT` and `MCP_TOOL_TIMEOUT` are all present in both binaries (contract/claude-grep-counts.txt). Their exact semantics beyond presence are **UNVERIFIED** here.

## Wire APIs

- **Anthropic Messages only**, plus Bedrock, Vertex and Foundry as 3P carriers of the Anthropic format.
- There are no `/v1/chat/completions` or `/v1/responses` strings and no `CLAUDE_CODE_USE_OPENAI` in the binary. The search found 0 matches.
- openai_responses and openai_chat are **not supported natively**. They would need a translating proxy.

## stream-json protocol (from embedded source)

- **System init.** Offset 193938302: `{type:"system",subtype:"init",cwd,session_id,tools,mcp_servers:[{name,status}],model,permissionMode,slash_commands,…}`
- **Result.** Offset 185489227: `{type:"result",subtype:"success"|error…,is_error,num_turns,result,stop_reason,duration_ms,duration_api_ms,total_cost_usd,usage,modelUsage,permission_denials,session_id,uuid,…}`. The error subtypes `error_during_execution` and `error_max_turns` are present as strings.
- **Assistant and user messages.** The `assistant` and `user` (tool_result) message types exist, but their exact field shapes were not extracted (**UNVERIFIED** here; they follow Agent SDK `SDKMessage`).
- **Stdin control requests** (with `--input-format stream-json`), from the print.ts handler at offsets 206526859 to 206579333:
  - `{type:"control_request",request_id,request:{subtype:"interrupt"}}`, which supports an optional `cancel_queued:true`
  - `end_session`
  - `set_permission_mode` (`{mode}`), which is validated and fails with `invalid_mode`
  - `set_model`
  - `set_cwd`
  - `apply_flag_settings` (`{settings}`)
  - `mcp_set_servers`

  Replies use `{type:"control_response",response:{subtype:"success",request_id,response}}` (offset 182096052).
- **SIGINT/SIGTERM** behaviour in -p mode: **UNVERIFIED**.

## MCP

- **stdio server schema.** Offset 179776509: `{type?:"stdio",command (min 1),args:string[] (default []),env?:Record<string,string>,timeout?,alwaysLoad?}`. This is passed as `{"mcpServers":{"<name>":{…}}}` via `--mcp-config`. The `mcpServers` wrapper key comes from the documented format and was not re-extracted (**UNVERIFIED**).
- **Tool naming.** VERIFIED at offset 179592445: `` `mcp__${vn(server)}__${tool}` ``. `vn` replaces `[^a-zA-Z0-9_-]` with `_` (offset 179591164). The tool part is not normalised in this prefix builder.
- With `alwaysLoad:true`, tools are never deferred behind tool search.

## Sessions

Sessions are stored as `$CLAUDE_CONFIG_DIR` (default `~/.claude`) `/projects/<sanitised-cwd>/<session-id>.jsonl`. **UNVERIFIED** here; this comes from known behaviour. `--resume <id>` must run in the same cwd and config dir.

## Summary answers

- **(a) Custom base URL with a placeholder key.** Set `ANTHROPIC_BASE_URL=http://127.0.0.1:<proxy>`, then either `ANTHROPIC_API_KEY=placeholder` or `--settings '{"apiKeyHelper":"echo placeholder"}'`. The helper is cached for 5 min, which `CLAUDE_CODE_API_KEY_HELPER_TTL_MS` overrides. With `--bare`, only these two auth sources are read.
- **(b) stdio MCP.** Use `--mcp-config <json> --strict-mcp-config`. Tools are exposed as `mcp__<server>__<tool>`.
- **(c) Interrupt.** Send a stdin `control_request` with `subtype:"interrupt"`. This requires `--input-format stream-json`.
- **(d) Resume.** Use `--resume <session-id>`, or `--session-id <uuid>` to pre-assign the ID. Use the same `CLAUDE_CONFIG_DIR` and cwd.
- **(e) Per-turn overrides.** Use the stdin control requests `set_model`, `set_permission_mode` and `apply_flag_settings` in a long-lived stream-json process. Otherwise pass per-process flags (`--model`, `--settings`, `--permission-mode`). Note that `--system-prompt-snapshot on` pins the system prompt across resume.

## Verified by conformance (2026-10-07)

`harnesses/claudecode/conformance_integration_test.go` runs the pinned binary (locally and in `steadmesh/seat-claudecode`) against a scripted Anthropic Messages endpoint: `-p --output-format stream-json --verbose` works in 2.1.289; `ANTHROPIC_API_KEY` with `ANTHROPIC_BASE_URL` reports `apiKeySource: "ANTHROPIC_API_KEY"`; the `mcpServers` wrapper key in `--mcp-config` is accepted and tools appear as `mcp__steadmesh__<tool>`; SIGINT to the process group ends a streaming turn; `--resume` restores the session in the same `HOME` and cwd.
