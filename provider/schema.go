package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func optStr(desc string) schema.StringAttribute {
	return schema.StringAttribute{Optional: true, Description: desc}
}

func reqStr(desc string) schema.StringAttribute {
	return schema.StringAttribute{Required: true, Description: desc}
}

func optBool(desc string) schema.BoolAttribute {
	return schema.BoolAttribute{Optional: true, Description: desc}
}

func optInt(desc string) schema.Int64Attribute {
	return schema.Int64Attribute{Optional: true, Description: desc}
}

func strList(desc string) schema.ListAttribute {
	return schema.ListAttribute{Optional: true, ElementType: types.StringType, Description: desc}
}

func reqStrList(desc string) schema.ListAttribute {
	return schema.ListAttribute{Required: true, ElementType: types.StringType, Description: desc}
}

func strMap(desc string) schema.MapAttribute {
	return schema.MapAttribute{Optional: true, ElementType: types.StringType, Description: desc}
}

func opsMap(desc string) schema.MapAttribute {
	return schema.MapAttribute{Optional: true, ElementType: types.ListType{ElemType: types.StringType}, Description: desc}
}

func keyed(desc string, attrs map[string]schema.Attribute) schema.MapNestedAttribute {
	return schema.MapNestedAttribute{
		Optional:     true,
		Description:  desc + " Keyed by stable key.",
		NestedObject: schema.NestedAttributeObject{Attributes: attrs},
	}
}

// specAttributes mirrors pkg/spec.OrganizationSpec field-for-field (except
// key, display_name and data_retention, which are resource attributes).
func specAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"culture_refs": strList("Ordered organisation culture instruction references (configmap:<name>/<key>#sha256:<digest>)."),
		"team_templates": keyed("Reusable team templates. Resolved by the provider before the object is written.", map[string]schema.Attribute{
			"extends":          optStr("Template this template extends."),
			"instruction_refs": strList("Ordered team instruction references."),
			"roles":            strMap("Role key to role instruction reference; seats select one with role_ref = \"role:<key>\"."),
			"shared_memory":    opsMap("Memory store key to operations granted to members of a team using this template."),
			"parameters":       strMap("Template parameters."),
		}),
		"teams": keyed("Concrete teams. Membership is declared on seats.", map[string]schema.Attribute{
			"template":         optStr("Team template to instantiate."),
			"instruction_refs": strList("Additional ordered team instruction references."),
			"shared_memory":    opsMap("Memory store key to operations granted to members."),
			"parameters":       strMap("Parameters overriding template parameters."),
			"access_profiles":  strList("Access profiles granted to every member."),
		}),
		"memory_stores": keyed("Memory stores.", map[string]schema.Attribute{
			"retention":     optStr("retain (default) or delete."),
			"backing_class": optStr("Backing store class (postgres)."),
			"capacity_mb":   optInt("Declared capacity in MB."),
		}),
		"shared_workspaces": keyed("Shared workspaces.", map[string]schema.Attribute{
			"storage_class": optStr("Storage class."),
			"access_mode":   reqStr("ReadWriteMany or ReadOnlyMany."),
			"size_gb":       optInt("Size in GB."),
			"retention":     optStr("retain (default) or delete."),
		}),
		"harness_profiles": keyed("Harness profiles.", map[string]schema.Attribute{
			"adapter":      reqStr("Harness adapter: claude-code, codex, pi or fake."),
			"image_digest": reqStr("Pinned harness image."),
			"model": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "The model the harness uses and the connection that serves it.",
				Attributes: map[string]schema.Attribute{
					"connection": reqStr("A model connection."),
					"id":         reqStr("Model identifier sent to the endpoint. The platform rejects requests from the seat for any other model."),
					"api":        optStr("anthropic_messages, openai_responses or openai_chat. When unset, the first API the harness speaks that the connection and model also serve."),
					"settings":   strMap("Harness-specific model settings: effort (claude-code); reasoning_effort (codex); thinking, context_window, max_tokens, reasoning (pi)."),
				},
			},
			"required_capabilities": strList("Capabilities the adapter must support."),
			"config":                strMap("Adapter configuration."),
		}),
		"execution_profiles": keyed("Execution profiles.", map[string]schema.Attribute{
			"backend":           reqStr("Sandbox backend (kubernetes)."),
			"service_class":     optStr("interactive or background."),
			"idle_policy":       optStr("warm, warm_then_stop or suspend."),
			"idle_timeout":      optStr("Idle timeout duration, at least 1m."),
			"cpu_request":       optStr("CPU request quantity."),
			"cpu_limit":         optStr("CPU limit quantity."),
			"memory_request":    optStr("Memory request quantity."),
			"memory_limit":      optStr("Memory limit quantity."),
			"workspace_size_gb": optInt("Per-seat workspace size in GB."),
			"storage_class":     optStr("Storage class of per-seat workspace volumes (cluster default when empty)."),
			"required_features": strList("Backend features that must be supported."),
		}),
		"sandbox_profiles": keyed("Sandbox profiles.", map[string]schema.Attribute{
			"network_policy_ref":    optStr("Network policy (deny_all_except_platform)."),
			"filesystem_policy_ref": optStr("Filesystem policy (workspace_only)."),
			"runtime_class":         optStr("Kubernetes RuntimeClass."),
			"required_enforcement":  strList("Enforcement features that must be active."),
		}),
		"access_profiles": keyed("Sandbox access profiles: practical access from a seat's sandbox, one plugin per field. Seats and teams name them; a seat gets the union. Without any, a seat reaches only the platform. See docs/sandbox.html.", map[string]schema.Attribute{
			"tools": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "Binaries the seat image must provide; the seat does not start without them.",
				Attributes:  map[string]schema.Attribute{"binaries": reqStrList("Binary names, e.g. git, gh, curl.")},
			},
			"egress": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "Hosts the seat may reach through the egress gateway (enable_egress). Changes apply live; removed hosts close open connections within seconds.",
				Attributes:  map[string]schema.Attribute{"hosts": reqStrList("example.com, *.example.com (subdomains) or host:port. Without a port, 443 and 80.")},
			},
			"network": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "Direct connections to IP ranges, enforced by NetworkPolicy.",
				Attributes: map[string]schema.Attribute{
					"rules": schema.ListNestedAttribute{
						Required:    true,
						Description: "Allowed ranges.",
						NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
							"cidr":     reqStr("IP range, e.g. 10.0.5.0/24."),
							"ports":    schema.ListAttribute{Optional: true, ElementType: types.Int64Type, Description: "Ports; empty allows every port."},
							"protocol": optStr("tcp (default), udp or sctp."),
						}},
					},
				},
			},
			"github": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "Repository access through a github connection.",
				Attributes: map[string]schema.Attribute{
					"connection":  reqStr("A github connection."),
					"repos":       reqStrList("owner/name repositories."),
					"permissions": schema.MapAttribute{Required: true, ElementType: types.StringType, Description: "contents, pull_requests, issues or metadata => read or write."},
					"delivery":    optStr("platform (default): operations through the platform; the credential never enters the sandbox. sandbox: git and gh in the sandbox receive a credential (a scoped, hour-long token with a GitHub App)."),
				},
			},
			"browser": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "A headless browser tool (needs a -browser seat image); its traffic goes through the egress gateway.",
				Attributes: map[string]schema.Attribute{
					"session": schema.SingleNestedAttribute{
						Optional:    true,
						Description: "A signed-in session to load.",
						Attributes:  map[string]schema.Attribute{"connection": reqStr("A browser_session connection.")},
					},
				},
			},
		}),
		"connections": keyed("Connections to external services. Secrets are references only.", map[string]schema.Attribute{
			"adapter":      reqStr("Connector adapter: slack, terminal, linear, anthropic, openai or model (any compatible model endpoint)."),
			"account_id":   optStr("Authorised account or workspace identity."),
			"endpoint_ref": optStr("Base URL override (fakes, self-hosted). For model connections, the API base, usually ending in /v1."),
			"model": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "What a model connection serves. Defaults for anthropic and openai; the model adapter must declare its APIs. Claims are verified at readiness.",
				Attributes: map[string]schema.Attribute{
					"apis":   strList("APIs the endpoint serves: anthropic_messages, openai_responses, openai_chat."),
					"auth":   optStr("How the credential is sent: bearer (default), x-api-key or header:<Name>."),
					"verify": optStr("Readiness check: request (default; a minimal request per API and model), models (list models) or none."),
					"models": schema.ListNestedAttribute{
						Optional:    true,
						Description: "Models the endpoint serves. When set, harness profiles may only select these, and readiness checks each.",
						NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
							"id":   reqStr("Model identifier."),
							"apis": strList("APIs this model is served on; empty means all of the connection's APIs."),
						}},
					},
				},
			},
			"secret_ref": optStr("vault:<path> or k8s:<secret-name>. Never a raw credential."),
			"ownership":  optStr("external (default) or managed."),
			"required":   optBool("Whether the connection must authenticate before the organisation is ready. By default model connections a harness uses and communication connections with channel bindings are required; others, such as a work tracker, are optional: their failures are reported as IntegrationsDegraded but never block readiness or internal work."),
			"config":     strMap("Adapter configuration, e.g. team_id for linear."),
		}),
		"seats": keyed("Seats: persistent agent identities.", map[string]schema.Attribute{
			"role_ref":          reqStr("Role instruction reference, or role:<key> resolved from the seat's team templates."),
			"teams":             strList("Ordered team memberships."),
			"harness_profile":   reqStr("Harness profile key."),
			"execution_profile": reqStr("Execution profile key."),
			"sandbox_profile":   reqStr("Sandbox profile key."),
			"personal_memory":   optStr("Personal memory store key."),
			"workspace": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "Workspace binding.",
				Attributes: map[string]schema.Attribute{
					"persistent": optBool("Persistent per-seat workspace."),
					"shared":     strList("Shared workspace keys to mount (requires a grant)."),
				},
			},
			"display_name":     optStr("Display name."),
			"instruction_refs": strList("Additional seat-scoped instruction references."),
			"access_profiles":  strList("Access profiles granted to this seat, in addition to its teams'."),
			"adopt_from":       optStr("Explicitly adopt the retained data of a retired seat ID."),
		}),
		"grants": keyed("Access grants.", map[string]schema.Attribute{
			"subject":    reqStr("seat:<key> or team:<key>."),
			"resource":   reqStr("connection:<key>, memory:<key> or workspace:<key>."),
			"operations": reqStrList("Allowed operations."),
			"targets":    strList("Optional target restrictions."),
		}),
		"message_routes": keyed("Permitted message routes.", map[string]schema.Attribute{
			"from":          reqStr("seat:<key> or team:<key>."),
			"to":            reqStr("seat:<key> or team:<key>."),
			"reply":         optBool("Recipient may reply within conversations opened on this route."),
			"bidirectional": optBool("Permit both directions."),
		}),
		"channel_bindings": keyed("Bindings of verified human identities to representative seats.", map[string]schema.Attribute{
			"connection":       reqStr("Communication connection key."),
			"external_user_id": reqStr("Verified external user ID."),
			"seat":             reqStr("Representative seat key."),
			"mode":             optStr("direct_message."),
		}),
		"work_publication": schema.SingleNestedAttribute{
			Optional:    true,
			Description: "Publish the work items of shared memory stores to a tracker so people can follow progress there. Optional: agents coordinate through memory and messages either way, and a failing tracker never blocks them. Personal stores are never published.",
			Attributes: map[string]schema.Attribute{
				"connection": reqStr("Tracker connection key, e.g. a Linear connection."),
				"stores":     reqStrList("Shared memory stores whose work items are published."),
			},
		},
	}
}

var resolvedSeatAttrTypes = map[string]attr.Type{
	"config_revision": types.StringType,
	"teams":           types.ListType{ElemType: types.StringType},
	"harness_profile": types.StringType,
	"role_ref":        types.StringType,
}

var conditionAttrTypes = map[string]attr.Type{
	"type":    types.StringType,
	"status":  types.StringType,
	"reason":  types.StringType,
	"message": types.StringType,
}

var representativeAttrTypes = map[string]attr.Type{
	"seat":             types.StringType,
	"seat_id":          types.StringType,
	"connection":       types.StringType,
	"adapter":          types.StringType,
	"external_user_id": types.StringType,
	"mode":             types.StringType,
}

var connectionDetailsAttrTypes = map[string]attr.Type{
	"organization_id": types.StringType,
	"namespace":       types.StringType,
	"status_endpoint": types.StringType,
	"representatives": types.MapType{ElemType: types.ObjectType{AttrTypes: representativeAttrTypes}},
}

func organizationSchema(ctx context.Context) schema.Schema {
	return schema.Schema{
		Version:     0,
		Description: "An organisation of persistent agents (one AgentOrganization object). The provider owns its declared fields; the controller owns seats and status.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Immutable organisation ID assigned by the platform (status.organizationID); the object UID until the controller has assigned one.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"key": schema.StringAttribute{
				Required:      true,
				Description:   "Stable organisation key; also the AgentOrganization object name. Changing it replaces the organisation.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"display_name": reqStr("Human-readable organisation name."),
			"spec": schema.SingleNestedAttribute{
				Required:    true,
				Description: "Typed organisation specification. Templates are resolved by the provider; the object receives the resolved specification.",
				Attributes:  specAttributes(),
			},
			"data_retention": optStr("Data retention on organisation deletion: retain (the default applied by the compiler) or delete."),
			"wait_for_ready": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "Wait for OperationalReady=True on the current generation after create and update.",
			},
			"manifest_digest": schema.StringAttribute{
				Computed:    true,
				Description: "Digest of the compiled effective manifest. Changes when the declaration, a template or the live object changes.",
			},
			"resolved_seats": schema.MapAttribute{
				Computed:    true,
				ElementType: types.ObjectType{AttrTypes: resolvedSeatAttrTypes},
				Description: "Compiled per-seat summary, so template changes show the affected seats in the plan. Keyed by seat key; each entry has role_ref, teams, harness_profile and config_revision (the seat's configuration revision).",
			},
			"conditions": schema.ListAttribute{
				Computed:    true,
				ElementType: types.ObjectType{AttrTypes: conditionAttrTypes},
				Description: "Observed status conditions, sorted by type. Each has type (e.g. OperationalReady, ConnectionsAuthenticated), status (True, False or Unknown), reason and message.",
			},
			"effective_revision": schema.StringAttribute{
				Computed:    true,
				Description: "Effective revision reported by the controller.",
			},
			"connection_details": schema.SingleNestedAttribute{
				Computed:    true,
				Description: "Stable, non-secret connection details reported by the controller.",
				Attributes: map[string]schema.Attribute{
					"organization_id": schema.StringAttribute{Computed: true, Description: "Immutable organisation ID assigned by the platform."},
					"namespace":       schema.StringAttribute{Computed: true, Description: "Namespace the organisation's seats run in."},
					"status_endpoint": schema.StringAttribute{Computed: true, Description: "Platform URL of the organisation's runtime status. It is on the internal API, which accepts only the controller's identity."},
					"representatives": schema.MapAttribute{
						Computed:    true,
						Description: "Representative seats by human, keyed by channel binding. Each has seat, seat_id, connection, adapter, external_user_id and mode, so you can tell people which identity reaches their representative.",
						ElementType: types.ObjectType{AttrTypes: representativeAttrTypes},
					},
				},
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Create: true, Update: true, Delete: true}),
		},
	}
}
