// Package provider implements the steadmesh Terraform provider (design §5).
package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/client-go/dynamic"
)

// providerData is handed to resources by Configure.
type providerData struct {
	client    orgClient
	namespace string
}

type steadmeshProvider struct {
	version string
	// newClient overrides client construction in tests.
	newClient func(kubeconfigPath, kubeContext string) (orgClient, error)
}

type providerModel struct {
	KubeconfigPath types.String `tfsdk:"kubeconfig_path"`
	KubeContext    types.String `tfsdk:"kube_context"`
	Namespace      types.String `tfsdk:"namespace"`
}

// New returns a provider factory.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &steadmeshProvider{version: version} }
}

func (p *steadmeshProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "steadmesh"
	resp.Version = p.version
}

func (p *steadmeshProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages agent organisations on a Kubernetes cluster running the steadmesh platform.",
		Attributes: map[string]schema.Attribute{
			"kubeconfig_path": schema.StringAttribute{
				Optional:    true,
				Description: "Path to the kubeconfig file. Defaults to the standard loading rules (KUBECONFIG, ~/.kube/config).",
			},
			"kube_context": schema.StringAttribute{
				Required:    true,
				Description: "Kubeconfig context to use. Required so that the ambient current-context is never used implicitly.",
			},
			"namespace": schema.StringAttribute{
				Required:    true,
				Description: "Organisation namespace, created by the foundation stage.",
			},
		},
	}
}

func (p *steadmeshProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for name, v := range map[string]types.String{"kubeconfig_path": cfg.KubeconfigPath, "kube_context": cfg.KubeContext, "namespace": cfg.Namespace} {
		if v.IsUnknown() {
			resp.Diagnostics.AddAttributeError(path.Root(name), "Unknown provider configuration", name+" must be known at plan time; configure the organisation stage from variables, not from resources created in the same run.")
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if cfg.KubeContext.ValueString() == "" {
		resp.Diagnostics.AddAttributeError(path.Root("kube_context"), "Missing kube_context", "kube_context must name a kubeconfig context; the ambient context is never used.")
		return
	}
	if cfg.Namespace.ValueString() == "" {
		resp.Diagnostics.AddAttributeError(path.Root("namespace"), "Missing namespace", "namespace must name the organisation namespace.")
		return
	}
	newClient := p.newClient
	if newClient == nil {
		newClient = defaultClient
	}
	c, err := newClient(cfg.KubeconfigPath.ValueString(), cfg.KubeContext.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Cannot use the steadmesh API", err.Error())
		return
	}
	data := &providerData{client: c, namespace: cfg.Namespace.ValueString()}
	resp.ResourceData = data
	resp.DataSourceData = data
}

func defaultClient(kubeconfigPath, kubeContext string) (orgClient, error) {
	rc, err := restConfig(kubeconfigPath, kubeContext)
	if err != nil {
		return nil, err
	}
	if err := checkAPI(rc); err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return &dynamicOrgClient{dyn: dyn}, nil
}

func (p *steadmeshProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{newOrganizationResource}
}

func (p *steadmeshProvider) DataSources(context.Context) []func() datasource.DataSource {
	return nil
}

// Address is the provider source address used in required_providers and in
// dev_overrides.
const Address = "registry.terraform.io/darcys22/steadmesh"
