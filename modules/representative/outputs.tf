output "seat_key" {
  value = local.seat_key
}

output "memory_store_key" {
  value = local.store_key
}

output "seats" {
  description = "Entry for spec.seats."
  value = {
    (local.seat_key) = {
      role_ref          = var.role_ref
      harness_profile   = var.harness_profile
      execution_profile = var.execution_profile
      sandbox_profile   = var.sandbox_profile
      personal_memory   = local.store_key
      display_name      = var.display_name
      instruction_refs  = length(var.instruction_refs) > 0 ? var.instruction_refs : null
      workspace         = { persistent = true }
    }
  }
}

output "memory_stores" {
  description = "Entry for spec.memory_stores: the representative's private store."
  value = {
    (local.store_key) = { retention = var.memory_retention }
  }
}

output "channel_bindings" {
  description = "Entry for spec.channel_bindings."
  value = {
    (var.human) = {
      connection       = var.connection
      external_user_id = var.external_user_id
      seat             = local.seat_key
      mode             = "direct_message"
    }
  }
}
