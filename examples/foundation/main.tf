# Foundation stage (design §5.3): namespaces, durable store, secret manager
# integration and credential Secrets. The kind cluster itself is created by
# examples/Makefile before this root runs.

resource "kubernetes_namespace_v1" "system" {
  metadata {
    name   = var.system_namespace
    labels = { "steadmesh.io/role" = "control-plane" }
  }
}

resource "kubernetes_namespace_v1" "organisation" {
  metadata {
    name   = var.organisation_namespace
    labels = { "steadmesh.io/role" = "organisation" }
  }
}

# ---------------------------------------------------------------- Postgres

module "postgres" {
  source           = "../../modules/postgres"
  namespace        = kubernetes_namespace_v1.system.metadata[0].name
  postgres_image   = var.postgres_image
  postgres_storage = var.postgres_storage
}

# ---------------------------------------------------------------- Vault (optional, dev mode)

resource "helm_release" "vault" {
  count      = var.enable_vault ? 1 : 0
  name       = "vault"
  repository = "https://helm.releases.hashicorp.com"
  chart      = "vault"
  version    = var.vault_chart_version
  namespace  = kubernetes_namespace_v1.system.metadata[0].name
  wait       = true
  timeout    = 300

  values = [yamlencode({
    injector = { enabled = false }
    server = {
      dev = { enabled = true }
    }
  })]

  set_sensitive {
    name  = "server.dev.devRootToken"
    value = var.vault_dev_root_token
  }
}

# ---------------------------------------------------------------- Credentials

resource "kubernetes_secret_v1" "slack" {
  metadata {
    name      = "slack-credentials"
    namespace = kubernetes_namespace_v1.system.metadata[0].name
  }
  data = {
    bot_token = var.slack_bot_token
    app_token = var.slack_app_token
  }
}

resource "kubernetes_secret_v1" "linear" {
  metadata {
    name      = "linear-credentials"
    namespace = kubernetes_namespace_v1.system.metadata[0].name
  }
  data = {
    api_key = var.linear_api_key
  }
}

# One Secret per model endpoint: <key>-credentials with key api_key.
resource "kubernetes_secret_v1" "model" {
  for_each = nonsensitive(toset(keys(var.model_api_keys)))
  metadata {
    name      = "${each.key}-credentials"
    namespace = kubernetes_namespace_v1.system.metadata[0].name
  }
  data = {
    api_key = var.model_api_keys[each.key]
  }
}
