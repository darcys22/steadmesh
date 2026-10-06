terraform {
  required_version = ">= 1.5.0"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.38"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.17"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

# Pinned to the local kind cluster. The ambient kubectl context is never used.
locals {
  kube_context = "kind-steadmesh"
}

provider "kubernetes" {
  config_path    = var.kubeconfig_path
  config_context = local.kube_context
}

provider "helm" {
  kubernetes {
    config_path    = var.kubeconfig_path
    config_context = local.kube_context
  }
}
