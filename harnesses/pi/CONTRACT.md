# CONTRACT: Pi coding agent (headless RPC)

Verified 2026-10-07.

## Pinned version and provenance

| Item | Value |
|---|---|
| npm package | `@earendil-works/pi-coding-agent@1.0.4` |
| npm tarball | `https://registry.npmjs.org/@earendil-works/pi-coding-agent/-/pi-coding-agent-1.0.4.tgz` |
| tarball sha256 | `04910bdae661a6529e9d6869b04006f6aad01186398b04a16c6d9793667b96c5` (`contract/npm-tarballs.sha256`) |
| How fetched | `npm pack --ignore-scripts`, then extracted. No install, no install scripts. |
| Source | `git clone --depth 1 --branch v1.0.4 https://github.com/earendil-works/pi.git` gives commit `7c10bd4337495ee613f2224843ecdf349b80d1df` (`src/pi`) |
| Docs | `packages/coding-agent/docs/` in the clone is byte-identical to `package/docs/` in the tarball (`diff -rq` reports no differences) |
| Binary run | `node package/dist/bundle/cli.js` (a self-contained bundle that runs without `node_modules`) on Node v26.8.1, macOS arm64. `--version` prints `1.0.4` (`contract/pi-version.txt`) |
| Entry point | `bin.pi = dist/bundle/cli.js` (`package/package.json`) |

In this file, source paths are relative to `src/pi/packages/` unless they say otherwise. Live-test artifacts are under `contract/pi-live/`, and the scripts that produced them are in `contract/scripts/`.

### Live test setup (`contract/scripts/`)

- `mock_llm.py` is a local HTTP mock on 127.0.0.1:18431. It logs the path, the auth headers, the declared tool names and the message roles of each request to `contract/pi-live/llm-requests.jsonl`.
  - `/v1/chat/completions` streams SSE. It returns a tool call to `mcp__bridge__echo` when the prompt contains "usetool" and plain text otherwise.
  - `/v1/responses` and `/v1/messages` return 401.
- `mcp_echo.py` is a minimal stdio MCP server named `bridge` with one tool, `echo`.
- `drive_rpc.py` and `abort_rpc.py` run `pi --mode rpc` and record every stdout record in `rpc-transcript.jsonl` and `abort-transcript.jsonl`.
- The agent config is in `contract/pi-live/agent/{models.json,mcp.json}`. The environment is isolated with `HOME`, `PI_CODING_AGENT_DIR`, `PI_OFFLINE=1` and `PI_TELEMETRY=0`.

## 1. CLI flags (RPC-relevant)

Evidence: `contract/pi-help.txt` (captured `--help`) and `coding-agent/src/cli/args.ts`.

| Flag | Verified | Evidence |
|---|---|---|
| `--mode rpc` (values `text`, `json`, `rpc`) | yes | args.ts:96-99; help |
| `--session <path\|id>` (file path or partial UUID) | yes, live: `--session 01a11509-602d` resumed the full session in a new process (`pi-live/resume-by-id.jsonl`: sessionId, messageCount 11, model restored to mockant) | args.ts:134 |
| `--session-id <id>` (exact id, created if it does not exist) | yes, from help and source | args.ts:136 |
| `--session-dir <dir>` (also env `PI_CODING_AGENT_SESSION_DIR`) | yes, live: files were written flat as `<ISO-ts>_<uuid>.jsonl` in the directory | args.ts:140; config.ts:586 |
| `--no-session` (ephemeral) | yes, live (used in abort test) | args.ts:132 |
| `--provider`, `--model provider/id[:thinking]`, `--thinking` | yes | help |
| `--approve`/`-a`, `--no-approve`/`-na` (project trust for this run) | yes | args.ts:235-237 |
| `--offline` / `PI_OFFLINE=1` | yes | help |
| `--no-mcp` | yes. It disables the built-in `mcp` extension (main.ts:784 `disabledBuiltinExtensions: parsed.noMcp ? ["mcp"]`) | |
| `--no-extensions`/`-ne` | **Caution:** it also disables built-in extensions, which include `mcp`, `codemode` and `tool-search` (help text; `extensions/index.ts:8-13`). Explicit `-e builtin:mcp` still loads (help: "explicit -e paths still work"; resource-loader.ts:724) | |
| `--tools`, `--exclude-tools`, `--no-builtin-tools`, `--append-system-prompt`, `--system-prompt` | yes | help |

RPC mode rejects `@file` prompt arguments (docs/rpc.md:20).

## 2. RPC protocol

Framing is strict JSONL. Split on LF only; do not split on U+2028/U+2029 (docs/rpc.md:50-56). Stdout carries protocol records only, and diagnostics go to stderr. Closing stdin triggers an orderly shutdown (docs/rpc.md:93). The process also handles SIGTERM (exit 143) and SIGHUP (exit 129) (rpc-mode.ts:366-380).

### Commands

The full union is in `coding-agent/src/modes/rpc/rpc-types.ts:20-74`. Every command takes an optional `id`, and the response echoes it.

| Command | Shape | Response `data` | Evidence |
|---|---|---|---|
| `prompt` | `{type:"prompt", message, images?, streamingBehavior?: "steer"\|"followUp"}` | `{disposition: "started"\|"queued"\|"handled"}` | rpc-types.ts:22,118; agent-session.ts:300-301; rpc-mode.ts:394-414 |
| `abort` | `{type:"abort"}` | none. Pi responds after the session is idle | rpc-types.ts:25,127; rpc-mode.ts:426-429; agent-session.ts:2420-2430 |
| `get_state` | `{type:"get_state"}` | `RpcSessionState {model?, thinkingLevel, isStreaming, isCompacting, steeringMode, followUpMode, sessionFile?, sessionId, sessionName?, autoCompactionEnabled, messageCount, pendingMessageCount}` | rpc-types.ts:96-109,138; rpc-mode.ts:448-464 |
| `switch_session` | `{type:"switch_session", sessionPath}` | `{cancelled: boolean}` | rpc-types.ts:61,199; rpc-mode.ts:603-609 |
| `new_session` | `{type:"new_session", parentSession?}` | `{cancelled: boolean}` | rpc-types.ts:27,135; rpc-mode.ts:435-442 |
| `set_model` | `{type:"set_model", provider, modelId}` | full Model object | rpc-types.ts:33 |
| `set_thinking_level` | `{type:"set_thinking_level", level}` | none | rpc-types.ts:38 |
| `steer`, `follow_up`, `clear_queue`, `compact`, `set_auto_retry`, `abort_retry`, `bash`, `abort_bash`, `get_messages`, `get_last_assistant_text`, `get_entries`, `fork`, `clone`, `get_tree`, `get_session_stats`, `set_session_name`, `get_commands`, `export_html`, `cycle_*`, `get_available_*` | see source | | rpc-types.ts:20-74 |

A failed command returns `{type:"response", command, success:false, error}` (rpc-types.ts:245). Malformed JSON returns `command:"parse"` with no `id` (docs/rpc.md:81-85).

The `prompt` response is emitted only after preflight. A failure after acceptance does not produce a second response; it appears in the event stream instead (rpc-mode.ts:394-414; docs/rpc-commands.md:40-42).

The following behaviour was observed live:

- Sending a `prompt` while busy without `streamingBehavior` returns `success:false, error:"Agent is already processing. Specify streamingBehavior ('steer' or 'followUp') to queue the message."` (`pi-live/abort-transcript.jsonl`; agent-session.ts:2002).
- `new_session` followed by `switch_session` to the earlier file restored `messageCount` 11 and `sessionId`.
- **Observation:** after the in-process `switch_session`, `get_state.model` showed the CLI-startup model (mockchat), not the session's last `model_change` (mockant). A new process started with `--session` did restore mockant. After a switch, a supervisor should send `set_model` explicitly instead of relying on the restored model.
- An empty new session was not written to disk until it had content: only one file existed after `new_session`.

### Events (stdout, no `id`)

Event types are defined in `agent/src/types.ts:514-529` (`AgentEvent`) and `coding-agent/src/core/agent-session.ts:191-233` (`AgentSessionEvent`). Docs: `docs/json.md`.

- **Lifecycle:** `agent_start`, `turn_start`, `message_start{message}`, `message_update{usage, assistantMessageEvent}`, `message_end{message}`, `turn_end{message, toolResults}`, `agent_end{messages, willRetry}`, `agent_settled`.
- **`assistantMessageEvent.type` values** (wire form is delta-only and drops `partial`):
  - `text_start`, `text_delta{delta}`, `text_end{content}`
  - `thinking_start`, `thinking_delta`, `thinking_end`
  - `toolcall_start{contentIndex,id,toolName}`, `toolcall_delta{delta}`, `toolcall_end{toolCall:{type:"toolCall",id,name,arguments}}`
  - `done`, `error` (docs/json.md:74-93)
- **Tool execution:**
  - `tool_execution_start{toolCallId, toolName, args}`
  - `tool_execution_update{toolCallId, toolName, args, partialResult}`
  - `tool_execution_end{toolCallId, toolName, result, isError}`
  - Each may carry an optional `parentToolCallId` for calls made from codemode (agent-session.ts:185-189).
- **Other:** `queue_update`, `compaction_start/end`, `auto_retry_start/end`, `summarization_retry_*`, `session_info_changed`, `thinking_level_changed`, `entry_appended`, `bash_execution_update{id?,delta}` (RPC only), and `extension_error` (RPC only, rpc-mode.ts:349).
- **Final signal:** `agent_settled` is the "no more automatic work" signal (agent-session.ts:1071-1072; docs/json.md:48). `agent_end` alone is not final, because retries, compaction or queued follow-ups can continue. If the prompt disposition was `"handled"`, no `agent_settled` follows (docs/rpc.md:67).
- **Live MCP tool-turn sequence** (`pi-live/rpc-transcript.jsonl`):
  1. `toolcall_start(toolName:"mcp__bridge__echo")`
  2. `toolcall_end`
  3. `message_end(stopReason:"toolUse")`
  4. `tool_execution_start{toolCallId:"call_1",toolName:"mcp__bridge__echo",args:{text:"hi"}}`
  5. `tool_execution_end{result:{content:[{type:"text",text:"echo:hi"}],details:{server:"bridge",tool:"echo"},structuredContent:{...}},isError:false}`
  6. `message_start/end(role:"toolResult")`
  7. `turn_end`
  8. `turn_start`
  9. … `agent_end{willRetry:false}`
  10. `agent_settled`
- **Live abort mid-stream:** `message_end(stopReason:"aborted", errorMessage:"Request was aborted")`, then `turn_end`, `agent_end`, `agent_settled`, and then the `abort` response.
- **Provider errors** (401 from the mock) surface as `message_end` with `stopReason:"error"` and `errorMessage`, followed by `agent_settled`, not as a command error.

## 3. models.json

- **Location:** `$PI_CODING_AGENT_DIR/models.json`, with the agent dir defaulting to `~/.pi/agent` (config.ts:585, 605-621). `sessions/`, `auth.json` and `settings.json` live in the same directory (config.ts:624-651). The env var name is computed as `${APP_NAME.toUpperCase()}_CODING_AGENT_DIR`, which is `PI_CODING_AGENT_DIR` for this package (help output).
- **Parsing and validation:** comments and BOM are stripped, and the file is validated with a TypeBox schema (model-config.ts:294-333). A schema error yields the "Invalid models.json schema" diagnostic.
- **Provider fields** (model-config.ts:242-253): `name?`, `baseUrl?`, `apiKey?`, `api?` (free string), `oauth?: "radius"`, `headers?: Record<string,string>`, `compat?`, `authHeader?: boolean` (adds `Authorization: Bearer <apiKey>`, provider-composer.ts:374-386), `models?: ModelDefinition[]`, `modelOverrides?`.
- **Model fields** (model-config.ts:199-216):
  - `id` (required), `name?`, `api?`, `baseUrl?`
  - `reasoning?`, `thinkingLevelMap?`
  - `input?: ("text"|"image")[]`, `inputLimits?`
  - `cost?`, `promptCache?`
  - `contextWindow?` (default 128000), `maxTokens?` (default 16384) (provider-composer.ts:242-243)
  - `samplingParams?`, `samplingParamsByThinkingLevel?`
  - `headers?`, `compat?`
  - A custom model requires `api` (at model or provider level) and `baseUrl`, or loading throws (provider-composer.ts:216-223).
- **`api` known values** (`ai/src/types.ts:17-27`): `openai-completions`, `mistral-conversations`, `openai-responses`, `azure-openai-responses`, `openai-codex-responses`, `anthropic-messages`, `bedrock-converse-stream`, `google-generative-ai`, `google-vertex`, `pi-messages`. Each has an implementation in `ai/src/api/<api>.ts`.
- **apiKey and header value syntax** (resolve-config-value.ts): a leading `!` runs the rest as a shell command and uses trimmed stdout (10 s timeout, stderr ignored, non-zero exit counts as unresolved, lines 185-196). `$NAME` and `${NAME}` interpolate environment variables, and `$$` and `$!` escape. Anything else is a literal.
- **Per-request versus cached:** there are two resolvers. `resolveConfigValue` caches command output for the process lifetime (lines 9-10, 145-151, 208-216). `resolveConfigValueUncached` and `resolveConfigValueOrThrow` do not cache (lines 221-251). models.json `apiKey` and `headers` go through the **uncached** path: provider-composer.ts:467 and 477 call `resolveConfigValueOrThrow`/`resolveHeadersOrThrow`, and `ModelRuntime.prepareRequest` calls `getAuth` on every request (model-runtime.ts:650-670). docs/models.md:64 says the same: "Commands in `models.json` run at request time and are not cached by Pi". **Verified live:** three consecutive chat requests carried `Bearer key-1`, `key-2` and `key-3`, and the counter file ended at 3 (`pi-live/llm-requests.jsonl`, `pi-live/keycounter`). The cached path (`resolveConfigValue`) is used for `auth.json` stored credentials (auth-storage.ts:266, 446) and for `resolveHeaders`.
- **Credential precedence:** runtime `--api-key`, then `auth.json`, then models.json `apiKey`, then env vars (docs/models.md:23).
- **Wire auth per API, observed live with `apiKey:"placeholder"`:**
  - `openai-completions`: `POST {baseUrl}/chat/completions` with `Authorization: Bearer <key>`. Custom `headers` are sent (`X-Extra: static-header`).
  - `openai-responses`: `POST {baseUrl}/responses` with `Authorization: Bearer placeholder`.
  - `anthropic-messages`: `POST {baseUrl}/v1/messages?beta=true` with `x-api-key: placeholder` and no Authorization header. baseUrl is the host root, without `/v1`.

## 4. mcp.json

- **Locations:** user-level `$PI_CODING_AGENT_DIR/mcp.json`. Project-level `.pi/mcp.json` is read only when the project is trusted (docs/mcp.md:32; extensions/mcp/cli.ts:42). The top-level key is `mcpServers`.
- **Stdio server fields** (core/mcp-servers.ts:59-87): `type?: "stdio"`, `command` (one executable, not a shell string), `args?`, `env?` (values may use `${NAME}` or a whole-value `!cmd`), `cwd?`. Shared fields: `exposure?`, `toolExposure?: Record<pattern, exposure>`, `enabled?`, `timeout?` (per-request seconds, default 60), `description?`. HTTP servers use `url`, `headers` and `oauth`. SSE is rejected (docs/mcp.md:64,79).
- **Exposure values:** `"codemode"` (the **default**), `"deferred"`, `"direct"`, `"hidden"`. `"codemode-deferred"` is an alias for `codemode`. Any other value is rejected with `exposure must be one of …` (mcp-servers.ts:52-57, 234-255).
  - `direct`: declared to the model like a built-in tool.
  - `codemode`: reachable only from the `codemode` script tool.
  - `deferred`: reachable via `tool_search` (docs/mcp.md:191-200).
- **Tool naming:** `mcp__<server>__<tool>`. Every character outside `[A-Za-z0-9_]` becomes `_`. Names are capped at 64 characters; a name that is too long or collides gets a `_<8-hex sha256>` suffix (extensions/mcp/tools.ts:49, 84-93). Server names must match `^[A-Za-z0-9_-]+$` (mcp-servers.ts:152). **Verified live:** with `"exposure":"direct"`, the request declared tools `["read","bash","edit","write","mcp__bridge__echo"]` and the model's call was executed against the stdio server.
- **Startup:** servers connect in the background. The first prompt waits up to 10 s only for servers with `direct` tools (`DEFAULT_STARTUP_WAIT_MS = 10_000`, extensions/mcp/index.ts:88-93; docs/mcp.md:100). On shutdown, Pi closes stdin, sends SIGTERM, then sends SIGKILL to the process group (docs/mcp.md:102).
- **Permissions:** MCP calls pass through the tool pipeline, and no built-in approval is applied (docs/mcp.md:252).

## 5. Approvals and trust prompts in RPC mode

- Pi has **no per-tool-call approval system** (docs/security.md:3: "it does not ask for approval before every tool call"). Gating exists only if an extension adds it.
- Project trust is the only built-in prompt. It is shown only when `hasUI` is true for the trust context; in RPC, JSON and print modes it never prompts. With no override, saved decision or extension, the default `"ask"` resolves to **untrusted**, so project `.pi/*` resources are skipped (core/project-trust.ts:77-88; docs/security.md:77-82).
  - To control trust deterministically, pass `--approve` (trust `.pi/` in the workdir) or `--no-approve` (ignore it).
  - Alternatively, set global `defaultProjectTrust: "always"|"never"` in `$PI_CODING_AGENT_DIR/settings.json` (settings-manager.ts:151).
- Extension dialogs are forwarded in RPC mode as `extension_ui_request` records (`select`, `confirm`, `input`, `editor`, …), and the client answers with `extension_ui_response`. Dialogs with a `timeout` auto-resolve (rpc-mode.ts:129-269; docs/rpc-extension-ui.md:10,25). Built-in extensions call `ui.select`/`ui.confirm` only from slash-command flows such as `/mcp` and `/llama` (extensions/mcp/index.ts:950; extensions/llama/index.ts:131). A supervisor should still answer any unexpected `extension_ui_request` with `{"type":"extension_ui_response","id":…,"cancelled":true}`.
- Network: `PI_OFFLINE=1` disables startup network activity such as model-catalog refreshes from pi.dev. `PI_TELEMETRY=0` (or setting `enableInstallTelemetry:false`) disables install/update telemetry and provider attribution headers (docs/environment-variables.md:84-86; docs/settings.md:167).

## 6. Node requirement

`"engines": {"node": ">=22.19.0"}` (package.json:105-107; also from `npm view`). Tested on Node v26.8.1.

## Harness mechanisms

| Concern | Mechanism |
|---|---|
| API support | `anthropic-messages`, `openai-responses` and `openai-completions` (Chat Completions) are all supported, and all three were exercised live against a local base URL. |
| (a) Custom base URL with a placeholder key while a proxy injects the real credential | In models.json, set a custom provider with `baseUrl`, `api` and `apiKey:"placeholder"`, plus optional static `headers`. Select it with `--provider X --model id` or `set_model`. Pi sends the placeholder as `Authorization: Bearer` (OpenAI APIs) or `x-api-key` (Anthropic). An `!command` apiKey is also re-executed per request if rotation inside Pi is ever needed. |
| (b) Tool bridge via stdio MCP | Native and built in. `$PI_CODING_AGENT_DIR/mcp.json` → `mcpServers.<name> {command,args,env,"exposure":"direct"}` gives tools named `mcp__<name>__<tool>`. `exposure:"direct"` must be set explicitly because the default is `codemode`. Do not pass `--no-extensions` or `--no-mcp`. |
| (c) Interrupt | `{"type":"abort"}`. The response arrives after the session is idle, preceded by `message_end(stopReason:"aborted")` … `agent_settled`. `clear_queue` drops queued steer/follow-up messages. |
| (d) Resume | `--session <file\|partial-uuid>` (with `--session-dir`) at spawn, or `switch_session{sessionPath}` in-process. Sessions are `<session-dir>/<ISO-ts>_<uuid>.jsonl`. `get_state` returns `sessionFile` and `sessionId`. |
| (e) Per-turn overrides | There are no per-prompt override fields. Send `set_model{provider,modelId}` and/or `set_thinking_level{level}` between prompts, while idle. Other settings (system prompt, tools) are startup flags only. |

## Contradiction check

- "Pi supports all three APIs": **confirmed** (source and live).
- "native MCP with exposure:\"direct\"": **confirmed**, with two caveats:
  1. `direct` is not the default. Omitting `exposure` gives `codemode`, which hides the tools behind the `codemode` script tool.
  2. MCP is a built-in *extension*, so `--no-extensions`, `--no-mcp` or `"extensions":["-builtin:mcp"]` silently removes it.

## UNVERIFIED

- Behaviour of `abort` during an MCP tool execution (only abort during LLM streaming was tested).
- `streamingBehavior:"steer"`/`"followUp"` delivery semantics; documented only, not exercised.
- Exact retry policy for 401 versus 5xx from the proxy. A 401 was not retried in the live run (`auto_retry_*` was not emitted), but the 5xx/429 behaviour was not tested.
- Whether `switch_session` restoring the CLI-startup model rather than the session's last model is intended; it was observed once.

## Verified by conformance (2026-10-07)

`harnesses/pi/conformance_integration_test.go` runs the pinned CLI (locally and in `steadmesh/seat-pi` on Node 22) against a scripted endpoint for each of `openai-responses`, `openai-completions` and `anthropic-messages`: MCP tool calls with `exposure:"direct"`, `abort` during a streaming request, resume with `--session` in a new process, and a static placeholder `apiKey` replaced per request by the seat's forwarder. Still unverified: `abort` during an MCP tool execution, steer/follow-up queueing, and retry behaviour on 429/5xx.
