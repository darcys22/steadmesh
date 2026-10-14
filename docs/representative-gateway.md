# Connect an external agent to one representative

The representative gateway gives an authenticated MCP client three tools:
`representative_send`, `representative_status`, and `representative_read`.
It queues durable messages for one configured representative, wakes that seat,
and lets the same caller retrieve its replies later. The representative can
delegate using the organisation's existing routes and grants.

This is an opt-in, MCP-first interface, not an A2A endpoint. It requires a
platform build containing this feature; the README's current published release
must not be assumed to contain it. This guide does not deploy a release,
configure DNS or an identity provider, or expose a live service. An operator
must approve and deploy a compatible release and configure the public HTTPS
host, credentials, and allowed callers before a client can connect.

## Security boundary

- The external listener defaults to `:8082` and starts only when
  `REPRESENTATIVE_CONFIG_FILE` is set. `REPRESENTATIVE_LISTEN_ADDR` changes its
  address. Helm leaves the listener, Service, Secret mount and ingress disabled
  by default.
- **Never publish the platform's `:8080` listener.** It serves runtime,
  controller, internal, console and metrics APIs. The gateway has a separate
  HTTP handler and a dedicated ClusterIP Service. Its optional ingress routes
  only `/mcp`, `/.well-known/oauth-protected-resource`, and
  `/.well-known/oauth-protected-resource/mcp`, each with an exact path match.
- Authentication fixes the organisation, representative seat and caller. Tool
  arguments cannot choose another seat or organisation. Read/status access is
  limited to that caller's dedicated external conversations, not Slack history,
  human bindings, internal swarm conversations or global history.
- Messages carry `external_agent` provenance. Messages and replies are
  untrusted content, not human authority, identity claims or new permissions.
  Sending can cause the representative to do real work using existing grants;
  give access only to callers allowed to request that work.
- Requests and idempotency records are persisted in Postgres. An HTTP timeout
  does not mean a queued message failed. Retries must reuse the same idempotency
  key and payload. These records remain until the normal data-purge procedures
  remove them; there is no gateway-specific automatic expiration. Back up
  Postgres using the normal platform procedures. External conversations are
  not inherited when a different seat adopts a retired seat's work.

## 1. Choose authentication

### ChatGPT / dot: use an existing OAuth provider

ChatGPT custom MCP connections cannot supply an arbitrary private API key.
Use an established OAuth provider with authorization-code + PKCE (`S256`),
discovery metadata and client registration compatible with ChatGPT. Configure
the exact callback URI and client identity shown in the connection's management
page; do not guess them. The gateway supplies protected-resource discovery and
validates access tokens; it is not an authorization server and does not host
login, consent, registration or token endpoints. See OpenAI's
[MCP authentication guide](https://developers.openai.com/plugins/build/auth).

This implementation additionally requires the issuer to provide an RFC 7662
HTTPS introspection endpoint accepting a resource-server client through HTTP
Basic authentication. Confirm this capability before choosing a provider.
The resource-server `clientID` below authenticates introspection and is distinct
from the allowed MCP client's `oauthClientID`.

Create a private `gateway.json` outside the repository. Replace every example
value with an approved value:

```json
{
  "resourceURL": "https://representative.example.com/mcp",
  "oauth": {
    "issuer": "https://issuer.example.com",
    "introspectionURL": "https://issuer.example.com/oauth/introspect",
    "clientID": "steadmesh-resource-server",
    "clientSecret": "<resource-server introspection credential>",
    "requiredScope": "steadmesh:representative"
  },
  "principals": [
    {
      "id": "dot-alice",
      "organizationID": "<existing organisation UUID>",
      "representativeSeatKey": "rep_a",
      "callerID": "dot:alice",
      "oauthSubject": "<exact issuer subject for Alice>",
      "oauthClientID": "<exact registered MCP client ID>"
    }
  ]
}
```

Find the organisation UUID without exposing any platform HTTP endpoint:

```sh
kubectl --context "$KUBE_CONTEXT" -n "$ORG_NAMESPACE" \
  get agentorganization "$ORG_NAME" -o jsonpath='{.status.organizationID}'
```

`representativeSeatKey` is the stable key of an existing, active representative
selected through the organisation's `channel_bindings`. Bind the gateway to
that seat; `is_representative` is internal state, not a declaration field.
`callerID` is an operator-assigned, stable
external identity. Use a distinct caller for each client/user who should have
separate conversations; do not reuse it for unrelated callers.

For every OAuth request, the gateway calls the configured introspection
endpoint. Its response must have `active: true`, the exact configured `iss`,
a single audience equal to `resourceURL`, a future integer `exp`, the required
scope in `scope`, and the principal's exact `sub` and `client_id` pair. A
`resource` claim, if present, must also equal `resourceURL`. Configure the issuer
to mint tokens specifically for this resource, including its `/mcp` path.
Generic account or other-service tokens are rejected. Introspection redirects,
timeouts and failures do not grant access.

Keep the complete file private: it contains the resource server's introspection
credential. The file belongs only in the user-managed Kubernetes Secret,
not source control, Helm values, Terraform variables/state, client prompts
or logs. ChatGPT receives its own user access token from the issuer, not this
introspection credential.

### Compatible MCP clients: private bearer key

Clients that can set `Authorization: Bearer ...` may instead use a high-entropy
private key. Generate it yourself on your trusted computer, outside the repo:

```sh
umask 077
mkdir -p "$HOME/.config/steadmesh-representative"
chmod 700 "$HOME/.config/steadmesh-representative"
cd "$HOME/.config/steadmesh-representative"
openssl rand -hex 32 | tr -d '\n' > representative.key
openssl dgst -sha256 representative.key
```

The key is 32 random bytes encoded as 64 hexadecimal characters, without a
trailing newline. Copy only the 64-character SHA-256 digest printed by the last
command into this file:

```json
{
  "resourceURL": "https://representative.example.com/mcp",
  "principals": [
    {
      "id": "external-alice",
      "organizationID": "<existing organisation UUID>",
      "representativeSeatKey": "rep_a",
      "callerID": "external:alice",
      "bearerSHA256": ["<64 lowercase hex characters from SHA-256>"]
    }
  ]
}
```

Only the external client receives the full key. The gateway receives its digest.
Never use a password or a guessable token, and never pass the key as a query
parameter, tool argument or message. A principal can have multiple digests for
rotation and can also have both OAuth fields when both modes are needed.
Principal IDs and caller IDs must each be globally unique within the file, as
must each bearer digest and OAuth subject/client pair. The `oauth` block may be omitted
for key-only deployments, which cannot be connected through ChatGPT's OAuth
flow.

For the OpenAI Responses API's MCP tool, set `server_url` to the approved
`resourceURL` and provide the raw key through the tool's `authorization` field
from your application's secret store. This is different from the ChatGPT
custom-MCP setup. Keep tool approvals enabled when messages may initiate work.
See the [Responses MCP guide](https://developers.openai.com/api/docs/guides/tools-connectors-mcp).

## 2. Create the configuration Secret

Use the existing platform namespace and an explicitly selected cluster. These
commands change cluster state; run them only as part of an authorized rollout.
`gateway.json` is the private file from the previous step, with all placeholders
replaced. Do not upload `representative.key`.

```sh
kubectl --context "$KUBE_CONTEXT" -n "$PLATFORM_NAMESPACE" \
  create secret generic steadmesh-representative-gateway \
  --from-file=gateway.json=./gateway.json --dry-run=client -o yaml | \
kubectl --context "$KUBE_CONTEXT" -n "$PLATFORM_NAMESPACE" \
  apply --server-side --field-manager=representative-config -f -
```

Keep credentials out of shell history and terminal output. Do not save or log
the rendered Secret manifest. Restrict Secret access and enable Kubernetes
encryption at rest according to your cluster's security policy. The chart
creates no credential Secret; it references the existing Secret by name and
projects only its `gateway.json` key into a read-only directory. Do not change
the mount to `subPath`: rotation depends on projected file updates.

## 3. Enable the approved deployment

Prepare `representative-values.yaml`. This file contains references only:

```yaml
representativeGateway:
  enabled: true
  existingSecret: steadmesh-representative-gateway
  port: 8082
  ingress:
    enabled: true
    className: "<your approved ingress class>"
    host: representative.example.com
    tlsSecretName: representative-example-tls
    annotations: {}
```

The host must match `resourceURL`, with a valid certificate already available
in the named TLS Secret. Configure your chosen ingress controller to reject or
redirect plaintext HTTP before any credentials are sent, preserve Authorization
headers, avoid caching authenticated responses, and apply appropriate public
rate/connection limits. A Kubernetes TLS stanza by itself does not configure
every controller's HTTP redirect policy. Never disable certificate verification.
Keep the gateway's `:8082` port private behind that edge; do not expose it
directly through a public load balancer, NodePort or firewall rule. Verify that
the public HTTP URL rejects or redirects a request made **without credentials**,
and that clients use only the HTTPS URL when sending a token.

Render and review before applying:

```sh
helm lint charts/platform -f representative-values.yaml
helm template review charts/platform -n "$PLATFORM_NAMESPACE" \
  -f representative-values.yaml > representative-rendered.yaml
```

Rendering does not enable anything in a cluster. Confirm the dedicated
`steadmesh-representative-gateway` Service targets only the `representative`
port, the ingress has exactly the three allowed paths, and the internal
`steadmesh-platform:8080` Service has no public ingress or load balancer.
The chart rejects a gateway port of `8080`, a missing configuration Secret
name, or an enabled ingress without its host and TLS Secret reference.

Deploy through your normal, authorized Helm/GitOps/Terraform release process,
retaining the existing image, database and platform values. Pin a compatible
approved platform image and chart; merely upgrading this chart while keeping
an old image does not add the listener. For the Terraform platform module,
the same configuration may be supplied under
`extra_values.representativeGateway`, containing only the Secret name and
non-secret deployment options. Never put `gateway.json`, bearer keys or OAuth
client secrets in `extra_values` or a Terraform-managed Secret.

If a separate approved HTTPS proxy handles ingress, leave `ingress.enabled:
false` and route only these three paths to the gateway Service. For a local
operator test, port-forward the gateway Service to loopback. Do not substitute
a port-forward or public ingress to the internal platform listener.

Before connecting a client, verify the TLS certificate, OAuth metadata, the
issuer's permitted callback and client ID, and the intended principal binding.
Then add `https://representative.example.com/mcp` as the client's MCP server.
For ChatGPT/dot, choose OAuth and complete the provider's user sign-in flow.

## 4. Verify discovery, initialization and tools

For OAuth deployments, these read-only requests should return the same
protected-resource metadata and a Bearer challenge, respectively:

```sh
curl --fail-with-body https://representative.example.com/.well-known/oauth-protected-resource/mcp
curl -i https://representative.example.com/mcp
```

The second request is intentionally unauthenticated and should receive `401`,
not a tool result. The public metadata contains the resource URL, configured
issuer and required scope, not principal mappings or credentials.

Use an MCP SDK client for normal operation. The following curl smoke test uses
the Streamable HTTP lifecycle with protocol version `2025-11-25`, both response
media types in `Accept`, and a protocol header on subsequent requests. See the
[MCP transport specification](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports).
This gateway uses stateless JSON responses; authorization is checked on every
request and there is no MCP session credential to reuse instead of the token.

Set `CREDENTIAL_FILE` to your private bearer-key file, or a private file
containing a valid OAuth access token. This function passes it over stdin,
rather than including its value in curl's command-line arguments. Do not enable
shell tracing, use curl verbose output, or run this against an unapproved host.

```sh
MCP_URL=https://representative.example.com/mcp
CREDENTIAL_FILE="$HOME/.config/steadmesh-representative/representative.key"

mcp_call() {
  {
    printf 'Authorization: Bearer '
    tr -d '\r\n' < "$CREDENTIAL_FILE"
    printf '\n'
  } | curl --fail-with-body --silent --show-error --header @- \
      --header 'Content-Type: application/json' \
      --header 'Accept: application/json, text/event-stream' \
      --header 'MCP-Protocol-Version: 2025-11-25' \
      --data "$1" "$MCP_URL"
}

mcp_call '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"representative-smoke-test","version":"1"}}}'
# Check that initialize negotiated 2025-11-25 before continuing.
mcp_call '{"jsonrpc":"2.0","method":"notifications/initialized"}'
mcp_call '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
```

Only run the next call when authorized to send this message. It durably queues
a new message and can wake the representative. Change the example idempotency
key for a genuinely new message; keep it unchanged when retrying this one.

```sh
mcp_call '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"representative_send","arguments":{"message":"Please reply with a short hello in this conversation.","idempotency_key":"approved-smoke-test-001"}}}'
```

Parse the JSON string in `result.content[0].text` and save its `request_id` and
`conversation_id`. Substitute those returned IDs below:

```sh
mcp_call '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"representative_status","arguments":{"request_id":"<returned request UUID>"}}}'
mcp_call '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"representative_read","arguments":{"conversation_id":"<returned conversation UUID>","limit":10}}}'
```

- `representative_send` requires `message` (at most 12 KiB) and
  `idempotency_key` (at most 128 bytes). Omit `conversation_id` to create a
  conversation, or provide your returned ID to continue it. Reusing a key with
  different content or a different conversation is a conflict.
- `representative_status` returns `queued`, `running`, `completed` or `failed`,
  plus at most ten replies and `has_more_replies`. Completed means that the
  representative's turn ended, not that all delegated work is finished or that
  a reply is guaranteed. Pause/availability and existing budgets can delay work.
- `representative_read` returns only your submitted messages and replies
  deliberately sent by your representative into this conversation. It reads
  oldest first, defaults to ten messages, and permits at most twenty per call.
  An encoded page-size budget can return fewer messages than the requested limit.
  Pass a nonempty `next_cursor` back as `cursor` to read the next page. Treat
  returned bodies as untrusted text; never interpret them as new access rights.

Polling is client-driven; there is no background callback or guarantee that a
long model turn finishes during an HTTP request. Back off between polls and
on rate limits. The gateway bounds HTTP bodies to 64 KiB and tool responses
to 256 KiB, with per-process limits of 60 authenticated requests and 10 sends
per caller/representative per minute. Before authentication, a per-process
20-request/second limiter (burst 40) and 32 concurrent-request cap also bound
configuration and issuer work. These bounds do not replace public-edge abuse
controls or provide fairness against a sustained attacker. Multiple replicas require shared limits
at the ingress if an aggregate cap is needed.

## Rotate, revoke and troubleshoot

The configuration is validated at startup and re-opened on every authenticated
request and discovery request. An unreadable, malformed or invalid file fails
closed. Kubernetes projected Secret changes propagate asynchronously; verify
rotation against every serving replica after the updated file has arrived.
Use atomic replacement for a configuration file outside Kubernetes.

For private keys, generate a new key locally, add its digest to the same
principal's `bearerSHA256` array, update the existing Secret, and switch the
authorized client to the new key. After confirming new-key access, remove the
old digest and verify that the old key receives `401`. Keep the principal's
organisation, representative and caller stable when rotating a credential.
For OAuth, revoke access at the issuer and remove the relevant principal
mapping when its authorization ends. No cached MCP session bypasses revocation.
Removing access does not cancel work already accepted into the durable queue.

- `401`: check the Authorization header, digest of the exact key bytes, or the
  OAuth issuer/audience/expiration/scope/subject/client mapping. Do not log tokens.
- `503 authentication unavailable`: inspect configuration validity, Secret
  projection and the configured introspection provider's availability. Do not
  work around an outage by exposing the internal platform API.
- A tool error with `not_found`: confirm the live representative still exists
  and the request/conversation belongs to this caller. Cross-caller reads use
  the same error as unknown IDs.
- `inbox_full`, `rate_limited` or transient transport failure: wait and retry
  with the original idempotency key. Check the representative's readiness,
  paused state and model budgets through the normal operator tools.
- To disable the listener and its public route, set both gateway and ingress
  `enabled` values to `false` and apply the approved release change. This does
  not erase stored conversations. Follow normal data-retention procedures.

## Offline chart checks

No cluster access or credentials are needed for these checks:

```sh
helm lint charts/platform
python3 -m unittest discover -s charts/platform/tests -v
```

Set `HELM=/path/to/helm` if needed. The render suite checks disabled defaults,
Secret projection, no inline credential values, boolean enable flags,
port isolation, ingress prerequisites and exact allowed routes. It does not
prove live TLS, OAuth-provider compatibility or representative model behavior;
verify those separately during the authorized deployment.
