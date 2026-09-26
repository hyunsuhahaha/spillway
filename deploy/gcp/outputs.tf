output "public_url" {
  description = "The URL users open (edge on the anchor VM)"
  value       = "http://${google_compute_address.anchor.address}"
}

output "anchor_internal_ip" {
  description = "Cloud Run reaches the DB router here over Direct VPC egress"
  value       = google_compute_instance.anchor.network_interface[0].network_ip
}

output "cloudrun_url" {
  value = google_cloud_run_v2_service.app.uri
}

output "registry" {
  value = local.registry
}

output "ssh" {
  value = "gcloud compute ssh spillway-anchor --zone ${var.zone} --project ${var.project}"
}

output "cloud_env" {
  description = "Values for deploy/cloud/.env"
  value       = <<-EOT
    PUBLIC_URL=http://${google_compute_address.anchor.address}
    SPILLWAY_IMAGE=${local.registry}/spillway:latest
    SPILLWAY_PG_IMAGE=${local.registry}/spillway-pg:latest
    GCP_PROJECT=${var.project}
    GCP_REGION=${var.region}
    CLOUDRUN_SERVICE=${google_cloud_run_v2_service.app.name}
  EOT
}
