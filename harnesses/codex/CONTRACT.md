# CONTRACT: Codex (openai/codex) — app-server harness

Verified 2026-10-07. Paths below are relative to this directory unless absolute.
`SRC` = `src/codex/codex-rs` (shallow clone of tag `rust-v0.160.1`, commit `d27764b82f7118f674371e6d6e76271d9d606edb`).

## Pinned version and provenance

| Item | Value |
|---|---|
| Version | `codex-cli 0.160.1` (`contract/codex-version.txt`) |
| npm wrapper | `@openai/codex@0.160.1`, tgz sha256 `d84454cfa82f61add78b3270073c6e254923e89657c7f0dd1a88cc5659b4e0c0` (JS launcher only; binaries are optionalDependencies aliased to `@openai/codex@0.160.1-<platform>`) |
| npm binary used for local runs | `@openai/codex@0.160.1-darwin-arm64`, tgz sha256 `93121cae6c2fe77cdb20f88f611783dbec77a3e241948ad018da120480a6f296`; extracted `vendor/aarch64-apple-darwin/bin/codex` sha256 `09fa44fdc37a5fc70dc1ace31235f90468a2e193d0e85f7552eab068ea2582be` |
| npm for pod | `@openai/codex@0.160.1-linux-x64` (`https://registry.npmjs.org/@openai/codex/-/codex-0.160.1-linux-x64.tgz`, integrity `sha512-sIDhqV+bsZKKVaVFVY5iB+pAzyOz2XRm3H1KXCsSJwGj5p98qnrT0Fweo1Hfe7WnyTp8mfBNdbVzUeO+mHYugA==`; not downloaded) |
| GitHub release assets (for pod) | `codex-x86_64-unknown-linux-musl.tar.gz` sha256 `9226581be592d18f7e7f740a352fdb63aa61e45e39f7eb9b09d3888c84bba33f`; `codex-aarch64-unknown-linux-musl.tar.gz` sha256 `f54dc5852042445bf41da3aa31156f3cb02f52c5a1a04074de73dc5598f7e1f7`; there is also a standalone `codex-app-server-<arch>-unknown-linux-musl.tar.gz`. These digests are **as reported by the GitHub API**; the assets were not downloaded and re-hashed (`contract/codex-github-release-assets.txt`). |
| Generated schema | `contract/schema/codex_app_server_protocol.v2.schemas.json` (stable; the `--experimental` variant was also generated but is not checked in), produced by `codex app-server generate-json-schema --out <dir>` with the darwin binary |

## 1. JSON-RPC protocol (app-server, stdio JSONL)

Transport is `codex app-server` (default `--listen stdio://`). The command is marked `[experimental]` in `--help` (`contract/codex-app-server-help.txt`). Messages are newline-delimited JSON-RPC **without** a `"jsonrpc":"2.0"` field (see the live capture).

### Methods (VERIFIED: schema, source and live run)
Method names are declared in `SRC/app-server-protocol/src/protocol/common.rs` (`contract/codex-method-decls.txt`). The full list is in `contract/codex-schema-methods.txt`.

| Method | Kind | Decl line | Params (schema) | Result |
|---|---|---|---|---|
| `initialize` | request | common.rs:499 | `{clientInfo:{name,version,title?}, capabilities?:{experimentalApi?, optOutNotificationMethods?, ...}}`, where `clientInfo` is required (`contract/schema/v1/InitializeParams.json`) | `{userAgent, codexHome, platformFamily, platformOs}` |
| `initialized` | client notification | (ClientNotification.json, the only one) | none | — |
| `thread/start` | request | common.rs:551 | `approvalPolicy, approvalsReviewer, baseInstructions, config (free-form object of config overrides), cwd, developerInstructions, ephemeral, model, modelProvider, personality, sandbox, serviceName, serviceTier, sessionStartSource, threadSource`, all optional | `{thread:{id, path, modelProvider, ...}, model, modelProvider, cwd, approvalPolicy, sandbox, reasoningEffort, ...}` |
| `thread/resume` | request | common.rs:557 | required `threadId`; optional overrides `approvalPolicy, sandbox, model, modelProvider, config, cwd, baseInstructions, developerInstructions, excludeTurns, personality, serviceTier`. Experimental-only: `path`, `history` | like thread/start |
| `turn/start` | request | common.rs:1032 | required `threadId`, `input: UserInput[]`; per-turn overrides `model, effort, approvalPolicy, sandboxPolicy, cwd, summary, outputSchema, personality, serviceTier` | `{turn:{id,status:"inProgress",...}}` |
| `turn/interrupt` | request | common.rs:1050 | required `{threadId, turnId}` | `{}` |

`UserInput` variants (`type`): `text` (`{type,text}`), `image`, `localImage` (`path`), `audio`, `localAudio`, `skill`, `mention`.

### Notifications (VERIFIED: ServerNotification.json and live run)
| Name | Decl | Payload (required fields) |
|---|---|---|
| `turn/started` | common.rs:1944 | `{threadId, turn}` |
| `turn/completed` | common.rs:1946 | `{threadId, turn}`; `turn.status` ∈ `completed, interrupted, failed, inProgress`; `turn.error` = `{message, codexErrorInfo?, additionalDetails?}` or null |
| `item/started` | common.rs:1950 | `{item, threadId, turnId, startedAtMs}` |
| `item/completed` | common.rs:1955 | `{item, threadId, turnId, completedAtMs}` |
| `item/agentMessage/delta` | common.rs:1960 | `{delta, itemId, threadId, turnId}` |
| `error` | common.rs:1918 | `{error, threadId, turnId, willRetry}` |
| also: `thread/started`, `thread/status/changed` (`{type:"active"\|"idle"}`), `item/mcpToolCall/progress` (1977), `mcpServer/startupStatus/updated` (1979), `warning`, `deprecationNotice`, `remoteControl/status/changed` | | |

Server→client **requests** that a headless supervisor must answer or avoid: `item/commandExecution/requestApproval`, `item/fileChange/requestApproval`, `item/tool/requestUserInput`, `mcpServer/elicitation/request`, `item/permissions/requestApproval`, `item/tool/call` (dynamic tools), `account/chatgptAuthTokens/refresh`, `attestation/generate`, legacy `applyPatchApproval`, `execCommandApproval`.

### approvalPolicy and sandbox enum strings (VERIFIED, `contract/schema/v2/ThreadStartParams.json`)
- `approvalPolicy` (`AskForApproval`): `"untrusted" | "on-request" | "never"` or `{"granular":{sandbox_approval, rules, mcp_elicitations, request_permissions?, skill_approval?}}`.
- `thread/start` / `thread/resume` `sandbox` (`SandboxMode`): **kebab-case** `"read-only" | "workspace-write" | "danger-full-access"`.
- `turn/start` `sandboxPolicy` (`SandboxPolicy`, tagged object): `{"type":"dangerFullAccess"}` | `{"type":"readOnly",networkAccess?}` | `{"type":"externalSandbox",networkAccess?}` | `{"type":"workspaceWrite",writableRoots?,networkAccess?,excludeTmpdirEnvVar?,excludeSlashTmp?}`.
- **Caution:** `"dangerFullAccess"` is valid only inside `turn/start.sandboxPolicy.type` and in responses. The `thread/start` response echoes `"sandbox":{"type":"dangerFullAccess"}` even though the request takes the string `"danger-full-access"` (`contract/codex-app-server-handshake.log`).

### mcpToolCall item (VERIFIED, `contract/schema/v2/ItemCompletedNotification.json` → `ThreadItem`)
```
{ "type":"mcpToolCall", "id":str, "server":str, "tool":str, "arguments":any,
  "status":"inProgress"|"completed"|"failed",
  "result": {"content":[...], "structuredContent"?:any, "_meta"?:any} | null,
  "error": {"message":str} | null,
  "durationMs"?:int|null, "readOnlyHint"?:bool|null, "pluginId"?, "appContext"?, "mcpAppUi"?, "mcpAppResourceUri"? }
```
Required fields: `arguments, id, server, status, tool, type`. Other `ThreadItem.type` values: `userMessage, hookPrompt, agentMessage, functionCallOutput, plan, reasoning, commandExecution, fileChange, dynamicToolCall, collabAgentToolCall, subAgentActivity, webSearch, imageView, sleep, imageGeneration, enteredReviewMode, exitedReviewMode, contextCompaction`.

### Live run (VERIFIED, `contract/codex-app-server-handshake.log`, driver `contract/codex-handshake.mjs`)
The run went initialize → initialized → thread/start(`approvalPolicy:"never"`, `sandbox:"danger-full-access"`) → turn/start("hi") → turn/interrupt. The provider was unreachable (127.0.0.1:9). Observed sequence:
`turn/started` → `item/started`/`item/completed`(userMessage) → interrupt response `{}` → `thread/status/changed{idle}` → `turn/completed{turn.status:"interrupted"}`.
**Completion signal for the supervisor: `turn/completed` (check `turn.status`).**

## 2. config.toml `model_providers` (VERIFIED)

Struct: `SRC/model-provider-info/src/lib.rs:134-202` (`ModelProviderInfo`, `#[schemars(deny_unknown_fields)]`). Fields: `name` (must be non-empty, `SRC/config/src/config_toml.rs:945`), `base_url` (:141), `model_catalog_url`, `env_key` (:146), `env_key_instructions`, `experimental_bearer_token` (:154), `auth` (:156), `gateway_oauth`, `aws` (bedrock only), `wire_api` (:163), `query_params` (:165), `http_headers` (:168), `env_http_headers` (:173), `request_max_retries`, `stream_max_retries`, `stream_idle_timeout_ms`, `websocket_connect_timeout_ms`, `requires_openai_auth` (default false), `supports_websockets`, `supports_standalone_web_search`.

- **wire_api:** the only variant is `responses` (lib.rs:103-107). `"chat"` is rejected at deserialize time (lib.rs:127) with `CHAT_WIRE_API_REMOVED_ERROR` (lib.rs:97). Captured CLI error (`contract/codex-wire-api-chat-rejected.txt`):
  ```
  Error: failed to load bootstrap configuration
  Caused by:
      `wire_api = "chat"` is no longer supported.
      How to fix: set `wire_api = "responses"` in your provider config.
      More info: https://github.com/openai/codex/discussions/7782
      in `model_providers.proxy.wire_api`
  ```
  Exit code 1. Any other unknown value gives serde `unknown_variant` with expected `["responses"]` (lib.rs:128).
- **base_url:** defaults to `https://api.openai.com/v1` (or the ChatGPT backend for ChatGPT auth) if unset (lib.rs:421-441).
- **http_headers:** static map, inserted as-is (lib.rs:393-402). **env_http_headers:** maps header → env var name; the header is skipped if the var is unset or blank (lib.rs:404-413). **query_params:** appended to the base URL (lib.rs:455-461).
- **env_key:** the env var is read at request-auth build time; it is an error if missing or blank (lib.rs:474-491).
- **experimental_bearer_token:** sent as `Authorization: Bearer <token>`. Precedence is `env_key` value first, then `experimental_bearer_token` (`SRC/model-provider/src/auth.rs:292-304`).
- **Reserved provider IDs:** built-in IDs (e.g. `openai`, `ollama`, `lmstudio`) cannot be redefined. The error is "Built-in providers cannot be overridden. Rename your custom provider" (`config_toml.rs:901-921`). Use a custom ID such as `steadmesh`.
- **`auth` (command-backed bearer token)**, schema `SRC/protocol/src/config_types.rs:568-596`:
  ```toml
  [model_providers.<id>.auth]
  command = "..."             # required, non-empty; bare name → PATH, relative path → cwd
  args = []                   # default []
  timeout_ms = 5000           # default 5_000 (:564), NonZero
  refresh_interval_ms = 300000 # default 300_000 (:565); 0 = never proactively refresh, only after a 401 retry
  cwd = "/abs"                # default "."
  ```
  - **Re-run during a long process: YES.** `BearerTokenRefresher::resolve` caches the token and re-runs the command when `fetched_at.elapsed() >= refresh_interval` (`SRC/login/src/auth/external_bearer.rs:33-54`). `refresh()` re-runs it unconditionally on the 401 path (:56-64). The command runs with stdin null, and trimmed stdout is the token. Empty output, non-zero exit or timeout are errors (:103-159).
  - It cannot be combined with `env_key`, `experimental_bearer_token` or `requires_openai_auth`. Error: `provider auth cannot be combined with experimental_bearer_token` (`lib.rs:362-389`; captured in `contract/codex-config-recommended.txt`).
  - It runs eagerly at app-server start: the log shows `Failed to resolve external auth: provider auth command ... failed to start` right after initialize (`contract/codex-strict-config.txt`).

## 3. MCP servers (VERIFIED)

`[mcp_servers.<name>]`: `RawMcpServerConfig` at `SRC/config/src/mcp_types.rs:377-440` (deny_unknown_fields).
- stdio: `command`, `args`, `env` (map), `env_vars`, `cwd`. HTTP: `url`, `bearer_token_env_var`, `http_headers`, `env_http_headers`.
- Startup: `startup_timeout_sec` (float) or `startup_timeout_ms` (int); `_sec` wins (mcp_types.rs:484-490). Default startup timeout is **30 s**, and the default `tool_timeout_sec` is **300 s** (`SRC/codex-mcp/src/rmcp_client.rs:105-106`).
- Top-level `mcp_optional_startup_grace_ms` defaults to 1000 ms. It is how long to wait for **optional** servers while building the initial tool catalog; `0` means wait each server's full startup timeout (`config_toml.rs:318-322`). `required = true` on a server makes it mandatory (`connection_manager.rs:270-274`). **Recommendation:** set `required = true` and `mcp_optional_startup_grace_ms = 0` on the bridge, or the bridge's tools may be missing from the first turn. (The exact catalog-exclusion behaviour after the grace period is from the doc comment and was not exercised: **partly UNVERIFIED**.)
- Other fields: `enabled`, `enabled_tools`, `disabled_tools`, `supports_parallel_tool_calls`, `default_tools_approval_mode` (`auto|prompt|writes|approve`, snake_case, mcp_types.rs:26-34), and per-tool `tools.<name>`.
- Accepted config dumped by `codex mcp get <name> --json`: `contract/codex-config-accepted.txt` and `contract/codex-config-recommended.txt`.
- **Tool name presentation to the model:** tools are sent as a Responses API **namespace** tool (`SRC/core/src/tools/handlers/mcp.rs:498-530`):
  `{"type":"namespace","name":"mcp__<server>","description":...,"tools":[{"type":"function","name":"<tool>",...}]}`.
  The namespace is `mcp__` + sanitized server name, where non-`[A-Za-z0-9_]` characters become `_` (`SRC/codex-mcp/src/tools.rs:22,138-142,228-235`; `codex-mcp/src/mcp/mod.rs:576`). The flattened name `mcp__<server>__<tool>` is capped at 128 bytes, with a SHA-1 suffix added on collision or overflow (tools.rs:225-226,268-310). The `non_prefixed_mcp_tool_names` feature, which drops the `mcp__` prefix, is UnderDevelopment and off by default (`SRC/features/src/lib.rs:1432-1437`).
  **Proxy implication:** a proxy that translates Responses requests to other backends must handle `type:"namespace"` tools and namespaced function calls. **UNVERIFIED:** the exact wire shape of the model's namespaced `function_call` output item.
- **Approvals for MCP tools under `approvalPolicy:"never"`:** if a tool needs approval, the call is **denied** ("MCP tool call requires approval, but approval policy is never", `SRC/core/src/mcp_tool_call.rs:1620-1623`).
  - Auto-approval applies when `default_tools_approval_mode = "approve"`, or when the policy is `never` and the sandbox has full disk write (`danger-full-access`) (`SRC/codex-mcp/src/mcp/mod.rs:91-110`).
  - Under `auto` mode, a tool needs approval unless `readOnlyHint=true`, or both `destructiveHint=false` and `openWorldHint=false` (mcp_tool_call.rs:2466-2497).
  - **Recommendation:** set `default_tools_approval_mode = "approve"` on the bridge server.

## 4. Sessions / rollouts (VERIFIED)
- Path: `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<ts>-<threadId>.jsonl` (`SRC/rollout/src/recorder.rs:1722-1743`; `SESSIONS_SUBDIR="sessions"`, `ARCHIVED_SESSIONS_SUBDIR="archived_sessions"` at `SRC/rollout/src/lib.rs:86-87`). The live run returned `thread.path=.../codex-home-ok/sessions/2026/10/07/rollout-2026-10-07T17-25-04-01a11509-....jsonl`. The first line is `session_meta`.
- Lookup for `thread/resume{threadId}`: the SQLite state DB (`$CODEX_HOME/state_5.sqlite`, `SRC/state/src/sqlite.rs:32`) is checked first, with a fallback to scanning filenames under `sessions/` (`SRC/rollout/src/list.rs:1415-1440,1668-1675`).
- Live (`contract/codex-app-server-resume.log`): a **fresh CODEX_HOME containing only `config.toml` + `sessions/` copied** resumed the thread successfully, so the filename fallback works. An unknown id gives `{"code":-32600,"message":"no rollout found for thread id <id>"}`. Resume without `excludeTurns:true` emits a `deprecationNotice`.
- **Persist the whole `$CODEX_HOME`** (sessions + `*.sqlite`) across pod restarts. The run also creates `goals_1/logs_2/memories_1/queue_1/thread_history_1.sqlite`, `shell_snapshots/` and `installation_id`.

## 5. Network egress besides the configured provider
| Source | Default | Disable | Evidence |
|---|---|---|---|
| Curated plugins sync: git clone of `https://github.com/openai/plugins.git`, or HTTP `https://chatgpt.com/backend-api/plugins/export/curated` | **ON** (`plugins` feature Stable, default_enabled true) | `[features] plugins = false` or `--disable plugins` | `SRC/core-plugins/src/startup_sync.rs:23-30`, `manager.rs:748-763`, `features/src/lib.rs:1457-1460`. **Observed live:** `$CODEX_HOME/.tmp/plugins-clone-*/.git` was created on the first run (`contract/codex-home-layout.txt`) and was absent with `--disable plugins`. |
| Metrics to Statsig OTLP (hard-coded endpoint + client key) | `metrics_exporter` defaults to `Statsig` (`SRC/config/src/types.rs:654-656`), but it is only used when analytics are enabled. app-server defaults analytics **off** unless `--analytics-default-enabled` (`--help`; `SRC/core/src/otel_init.rs:70-77`) | `[analytics] enabled=false` and `[otel] metrics_exporter="none"` (belt and braces) | `SRC/otel/src/config.rs:10-34` |
| Analytics events `{base}/codex/analytics-events/events` | follows `analytics.enabled` | as above | `SRC/analytics/src/client.rs:154` |
| Update checks (npm registry, Homebrew) | TUI only (`tui/src/updates.rs`, `tui/src/npm_registry.rs`); not app-server | `check_for_update_on_startup=false` | config_toml.rs:520-523 |
| Model list refresh | goes to the **configured provider** (`/models`); without catalog metadata it logs `Model metadata for <model> not found. Defaulting to fallback metadata` (live) | supply `model_catalog_json` (path) to avoid it | config_toml.rs:400 |
| Feedback upload | only on the explicit `feedback/upload` request | `[feedback] enabled=false` | config_toml.rs:532-534 |

The recommended config was accepted by `codex app-server --strict-config` (`contract/codex-strict-config.txt`, file `contract/codex-config-recommended.toml`). The auth-command variant is in `contract/codex-config-auth-command.toml`. `--strict-config` is rejected for `codex mcp`.

## 6. Mechanism summary
- **(a) Custom base URL + placeholder key:** `model_provider="steadmesh"` with `[model_providers.steadmesh] base_url="http://127.0.0.1:<port>/v1"`, `wire_api="responses"`, `experimental_bearer_token="placeholder"` (or `env_key`). The proxy replaces the `Authorization` header. Alternatively `auth.command` (refreshed every `refresh_interval_ms`, default 5 min).
- **(b) Tool bridge:** `[mcp_servers.<n>]` stdio. Tools are exposed as namespace `mcp__<n>` / tool `<t>`. Set `required=true` and `default_tools_approval_mode="approve"`.
- **(c) Interrupt:** `turn/interrupt{threadId,turnId}` → `turn/completed` with `status:"interrupted"` (verified live).
- **(d) Resume:** `thread/resume{threadId}` with the same `CODEX_HOME` (verified live).
- **(e) Per-turn overrides:** `turn/start` accepts `model`, `effort`, `approvalPolicy`, `sandboxPolicy`, `cwd`, `summary`, `outputSchema`. Thread-level `config` object on `thread/start`/`thread/resume` for arbitrary config keys; process-level `-c key=value`.

## UNVERIFIED
- An end-to-end MCP tool call through a real model (no live model was used). The mcpToolCall shape is schema-derived only.
- The wire shape of namespaced function_call items in Responses streams sent to the proxy.
- Behaviour of the GitHub linux-musl binaries (not downloaded); the digests are from the GitHub API.
- Whether `thread/start.approvalPolicy` overrides `config.approval_policy` for the MCP auto-approve check in `mcp_permission_prompt_is_auto_approved` (likely, since thread params are applied as config overrides, but not traced).

## Verified by conformance (2026-10-07)

`harnesses/codex/conformance_integration_test.go` (`make conformance`, `make conformance-images`) runs the pinned binary (darwin-arm64 locally, linux-arm64 in `steadmesh/seat-codex`) against a scripted Responses endpoint. It settles these items from the list above:
- **MCP tool call through a model: verified.** The model's `function_call` item carries `"namespace":"mcp__steadmesh"` and `"name":"<tool>"` (e.g. `memory_write`); Codex executes it through the stdio bridge and sends a `function_call_output` on the next request (`SRC/core/src/tools/router.rs:250-258` builds the tool name from namespace and name).
- **`thread/start.approvalPolicy:"never"` with `danger-full-access` and `default_tools_approval_mode="approve"`:** the bridge's tools run without an approval request.
- The Linux binaries are installed from npm (`@openai/codex@0.160.1`, the registry's integrity check), not the GitHub release assets.
