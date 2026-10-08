terraform {
  required_providers {
    steadmesh = {
      source  = "darcys22/steadmesh"
      version = "~> 0.2.0"
    }
  }
}

provider "steadmesh" {
  kubeconfig_path = "~/.kube/config"
  kube_context    = "my-cluster" # always explicit: the current context is never used
  namespace       = "acme-org"   # the organisation namespace
}
