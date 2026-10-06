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

locals {
  pg_name   = "steadmesh-postgres"
  pg_labels = { "app.kubernetes.io/name" = local.pg_name, "app.kubernetes.io/part-of" = "steadmesh" }
  pg_user   = "steadmesh"
  pg_db     = "steadmesh"
}

resource "random_password" "postgres" {
  length  = 32
  special = false
}

resource "kubernetes_secret_v1" "postgres" {
  metadata {
    name      = local.pg_name
    namespace = kubernetes_namespace_v1.system.metadata[0].name
  }
  data = {
    POSTGRES_USER     = local.pg_user
    POSTGRES_PASSWORD = random_password.postgres.result
    POSTGRES_DB       = local.pg_db
  }
}

# Connection URL consumed by the platform service and controller.
resource "kubernetes_secret_v1" "db" {
  metadata {
    name      = "steadmesh-db"
    namespace = kubernetes_namespace_v1.system.metadata[0].name
  }
  data = {
    url = "postgres://${local.pg_user}:${random_password.postgres.result}@${local.pg_name}.${var.system_namespace}.svc:5432/${local.pg_db}?sslmode=disable"
  }
}

resource "kubernetes_service_v1" "postgres" {
  metadata {
    name      = local.pg_name
    namespace = kubernetes_namespace_v1.system.metadata[0].name
    labels    = local.pg_labels
  }
  spec {
    selector = local.pg_labels
    port {
      name        = "postgres"
      port        = 5432
      target_port = 5432
    }
  }
}

resource "kubernetes_stateful_set_v1" "postgres" {
  metadata {
    name      = local.pg_name
    namespace = kubernetes_namespace_v1.system.metadata[0].name
    labels    = local.pg_labels
  }
  spec {
    service_name = kubernetes_service_v1.postgres.metadata[0].name
    replicas     = 1
    selector {
      match_labels = local.pg_labels
    }
    template {
      metadata {
        labels = local.pg_labels
      }
      spec {
        security_context {
          run_as_user  = 70 # postgres user in the alpine image
          run_as_group = 70
          fs_group     = 70
        }
        container {
          name  = "postgres"
          image = var.postgres_image
          port {
            name           = "postgres"
            container_port = 5432
          }
          env_from {
            secret_ref {
              name = kubernetes_secret_v1.postgres.metadata[0].name
            }
          }
          env {
            name  = "PGDATA"
            value = "/var/lib/postgresql/data/pgdata"
          }
          readiness_probe {
            exec {
              command = ["pg_isready", "-U", local.pg_user, "-d", local.pg_db]
            }
            period_seconds = 5
          }
          liveness_probe {
            exec {
              command = ["pg_isready", "-U", local.pg_user, "-d", local.pg_db]
            }
            initial_delay_seconds = 30
            period_seconds        = 10
          }
          resources {
            requests = { cpu = "100m", memory = "256Mi" }
            limits   = { cpu = "1", memory = "1Gi" }
          }
          volume_mount {
            name       = "data"
            mount_path = "/var/lib/postgresql/data"
          }
        }
      }
    }
    volume_claim_template {
      metadata {
        name = "data"
      }
      spec {
        access_modes = ["ReadWriteOnce"]
        resources {
          requests = { storage = var.postgres_storage }
        }
      }
    }
  }
  wait_for_rollout = true
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

resource "kubernetes_secret_v1" "anthropic" {
  metadata {
    name      = "anthropic-credentials"
    namespace = kubernetes_namespace_v1.system.metadata[0].name
  }
  data = {
    api_key = var.anthropic_api_key
  }
}
