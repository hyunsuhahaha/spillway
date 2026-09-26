# Spillway — GCP cloud site.
#
#   anchor VM (e2-small)  : edge + control plane + DB router + standby Postgres
#                           (docker compose from deploy/cloud, joined to the tailnet)
#   Cloud Run service     : burst instances, scale-to-zero, reach the DB router
#                           over Direct VPC egress (no public DB port)
#
# Two-phase apply (the image must exist before Cloud Run can use it):
#   terraform apply -target=google_artifact_registry_repository.spillway -target=google_project_service.services
#   ../../scripts/push-images.sh gcp <region>-docker.pkg.dev/<project>/spillway
#   terraform apply
terraform {
  required_version = ">= 1.6"
  required_providers {
    google = { source = "hashicorp/google", version = ">= 6.0" }
  }
}

provider "google" {
  project = var.project
  region  = var.region
}

locals {
  registry  = "${var.region}-docker.pkg.dev/${var.project}/spillway"
  app_image = var.app_image != "" ? var.app_image : "${local.registry}/spillway:latest"
}

resource "google_project_service" "services" {
  for_each           = toset(["run.googleapis.com", "compute.googleapis.com", "artifactregistry.googleapis.com", "monitoring.googleapis.com"])
  service            = each.value
  disable_on_destroy = false
}

resource "google_artifact_registry_repository" "spillway" {
  location      = var.region
  repository_id = "spillway"
  format        = "DOCKER"
  depends_on    = [google_project_service.services]
}

data "google_compute_subnetwork" "default" {
  name   = "default"
  region = var.region
}

# ------------------------------------------------------------------ identities

# Runtime identity of the Cloud Run burst instances (no permissions needed).
resource "google_service_account" "run" {
  account_id   = "spillway-run"
  display_name = "Spillway Cloud Run burst instances"
}

# Identity of the anchor VM: the control plane uses it to scale Cloud Run and
# the VM uses it to pull images from Artifact Registry.
resource "google_service_account" "anchor" {
  account_id   = "spillway-anchor"
  display_name = "Spillway anchor VM (control plane)"
}

resource "google_project_iam_member" "anchor_run" {
  project = var.project
  role    = "roles/run.developer"
  member  = "serviceAccount:${google_service_account.anchor.email}"
}

resource "google_project_iam_member" "anchor_registry" {
  project = var.project
  role    = "roles/artifactregistry.reader"
  member  = "serviceAccount:${google_service_account.anchor.email}"
}

resource "google_project_iam_member" "anchor_monitoring" {
  project = var.project
  role    = "roles/monitoring.viewer"
  member  = "serviceAccount:${google_service_account.anchor.email}"
}

# Updating a Cloud Run service requires acting as its runtime identity.
resource "google_service_account_iam_member" "anchor_act_as_run" {
  service_account_id = google_service_account.run.name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.anchor.email}"
}

# ------------------------------------------------------------------ anchor VM

resource "google_compute_firewall" "public" {
  name          = "spillway-public"
  network       = "default"
  source_ranges = ["0.0.0.0/0"]
  target_tags   = ["spillway-anchor"]
  allow {
    protocol = "tcp"
    ports    = ["80", "8090"]
  }
}

# The DB router is only reachable from inside the VPC (Cloud Run egress).
resource "google_compute_firewall" "dbrouter" {
  name          = "spillway-dbrouter-vpc"
  network       = "default"
  source_ranges = [data.google_compute_subnetwork.default.ip_cidr_range]
  target_tags   = ["spillway-anchor"]
  allow {
    protocol = "tcp"
    ports    = ["6432"]
  }
}

resource "google_compute_address" "anchor" {
  name   = "spillway-anchor"
  region = var.region
}

resource "google_compute_instance" "anchor" {
  name         = "spillway-anchor"
  zone         = var.zone
  machine_type = var.machine_type
  tags         = ["spillway-anchor"]

  boot_disk {
    initialize_params {
      image = "ubuntu-os-cloud/ubuntu-2404-lts-amd64"
      size  = 20
    }
  }

  network_interface {
    network    = "default"
    subnetwork = data.google_compute_subnetwork.default.self_link
    access_config {
      nat_ip = google_compute_address.anchor.address
    }
  }

  service_account {
    email  = google_service_account.anchor.email
    scopes = ["cloud-platform"]
  }

  metadata_startup_script = <<-EOT
    #!/bin/bash
    set -e
    if ! command -v docker >/dev/null; then
      apt-get update
      apt-get install -y docker.io docker-compose-v2
      systemctl enable --now docker
    fi
    gcloud auth configure-docker ${var.region}-docker.pkg.dev --quiet || true
    mkdir -p /opt/spillway
  EOT

  depends_on = [google_project_service.services]
}

# ------------------------------------------------------------------ Cloud Run

resource "google_cloud_run_v2_service" "app" {
  name                = "spillway-app"
  location            = var.region
  ingress             = "INGRESS_TRAFFIC_ALL"
  deletion_protection = false

  template {
    service_account = google_service_account.run.email
    scaling {
      min_instance_count = 0
      max_instance_count = var.cloudrun_max_instances
    }
    vpc_access {
      egress = "PRIVATE_RANGES_ONLY"
      network_interfaces {
        network    = "default"
        subnetwork = "default"
      }
    }
    containers {
      image = local.app_image
      args  = ["app"]
      ports {
        container_port = 8080
      }
      env {
        name  = "SITE"
        value = "cloud-gcp"
      }
      env {
        name  = "DB_URL"
        value = "postgres://spillway:${var.app_db_password}@${google_compute_instance.anchor.network_interface[0].network_ip}:6432/spillway?sslmode=disable"
      }
      env {
        name  = "WORK_MS"
        value = tostring(var.work_ms)
      }
      env {
        name  = "MAX_INFLIGHT"
        value = tostring(var.max_inflight)
      }
      resources {
        limits = { cpu = "1", memory = "512Mi" }
      }
      startup_probe {
        http_get {
          path = "/api/whoami"
        }
        period_seconds    = 1
        failure_threshold = 30
      }
    }
  }

  # The control plane changes the minimum instance count at runtime.
  lifecycle {
    ignore_changes = [scaling, template[0].scaling[0].min_instance_count, client, client_version]
  }

  depends_on = [google_project_service.services]
}

# The edge calls the service URL directly.
resource "google_cloud_run_v2_service_iam_member" "public" {
  name     = google_cloud_run_v2_service.app.name
  location = var.region
  role     = "roles/run.invoker"
  member   = "allUsers"
}
