terraform {
  required_version = ">= 1.5.0"
  required_providers {
    steadmesh = {
      # Built locally (make -C .. provider) and selected through dev_overrides
      # in the CLI config that examples/Makefile generates.
      source = "darcys22/steadmesh"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.38"
    }
  }
}

provider "steadmesh" {
  kubeconfig_path = var.kubeconfig_path
  kube_context    = "kind-steadmesh"
  namespace       = var.namespace
}

# Publishes instruction bundles as ConfigMaps in the organisation namespace.
provider "kubernetes" {
  config_path    = var.kubeconfig_path
  config_context = "kind-steadmesh"
}
