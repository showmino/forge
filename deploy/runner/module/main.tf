# The resources of one service in one environment. forge-runner runs this
# module for every plan and apply; forge-api gives it the variables and keeps
# its state (the "http" backend, configured through TF_HTTP_* variables).
#
# What a developer may set is in var.resources. Everything else here is the
# platform's choice: the Postgres image, the storage class, the pod sizes.
# The policy rules in policy/resources.rego check every plan of this module.
terraform {
  required_version = ">= 1.12.0"

  backend "http" {}

  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "3.3.0"
    }
  }
}

# In the runner Job the provider uses the pod's service account.
provider "kubernetes" {}

variable "service" {
  type        = string
  description = "The service's id, for example payments."
}

variable "environment" {
  type        = string
  description = "staging or production."
}

variable "namespace" {
  type        = string
  description = "The service's namespace in that environment: <service>-<environment>."
}

variable "resources" {
  type = list(object({
    name       = string
    type       = string
    storage_gi = number
    instances  = number
  }))
  description = "The resources forge.yaml declares, worked out for the environment."
}

locals {
  databases = { for r in var.resources : r.name => r if r.type == "postgres" }
}

# One CloudNativePG cluster per database. CloudNativePG writes the Secret
# <name>-app with the connection string; this module never reads it, so no
# password enters the state or a plan.
resource "kubernetes_manifest" "database" {
  for_each = local.databases

  manifest = {
    apiVersion = "postgresql.cnpg.io/v1"
    kind       = "Cluster"
    metadata = {
      name      = each.key
      namespace = var.namespace
      labels = {
        "app.kubernetes.io/managed-by" = "forge"
        "forge.dev/service"            = var.service
        "forge.dev/resource"           = each.key
      }
    }
    spec = {
      instances = each.value.instances
      # The same image forge-db runs (deploy/k3s/database.yaml.tmpl).
      imageName = "ghcr.io/cloudnative-pg/postgresql:18.4-system-trixie"
      storage = {
        size = "${each.value.storage_gi}Gi"
        # The volumes outlive the cluster: a deleted database can be recovered by hand.
        storageClass = "longhorn-retain"
      }
      resources = {
        requests = { cpu = "50m", memory = "256Mi" }
        limits   = { memory = "512Mi" }
      }
    }
  }

  # An apply ends when the database is usable: healthy, with every instance
  # ready. The phase alone is not enough: right after a change it still reads
  # healthy from before.
  wait {
    fields = {
      "status.phase"          = "Cluster in healthy state"
      "status.readyInstances" = tostring(each.value.instances)
    }
  }

  timeouts {
    create = "10m"
    update = "10m"
    delete = "5m"
  }
}

output "secrets" {
  description = "For each database, the Secret holding its connection string."
  value       = { for name, _ in local.databases : name => "${name}-app" }
}
