# Records for one human's personal representative (design §9.4): the seat,
# its private memory store and the channel binding to the verified human.
# Plain data; merge the outputs into the organisation spec.

terraform {
  required_version = ">= 1.5.0"
}

locals {
  seat_key  = coalesce(var.seat_key, "representative_${var.human}")
  store_key = coalesce(var.memory_store_key, "rep_${var.human}")
}
