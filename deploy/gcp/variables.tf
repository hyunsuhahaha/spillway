variable "project" {
  description = "GCP project id"
  type        = string
}

variable "region" {
  description = "Region for Cloud Run, Artifact Registry and the anchor VM (Seoul by default)"
  type        = string
  default     = "asia-northeast3"
}

variable "zone" {
  description = "Zone of the anchor VM"
  type        = string
  default     = "asia-northeast3-a"
}

variable "machine_type" {
  description = "Anchor VM machine type"
  type        = string
  default     = "e2-small"
}

variable "app_image" {
  description = "Image for Cloud Run (defaults to <region>-docker.pkg.dev/<project>/spillway/guestbook:latest)"
  type        = string
  default     = ""
}

variable "app_db_password" {
  description = "APP_DB_PASSWORD shared with the local site"
  type        = string
  sensitive   = true
}

variable "cloudrun_max_instances" {
  description = "Upper bound for Cloud Run autoscaling"
  type        = number
  default     = 10
}

variable "work_ms" {
  description = "Simulated CPU work per page, same as the local site"
  type        = number
  default     = 25
}

variable "max_inflight" {
  description = "Per-instance concurrency limit, same as the local site"
  type        = number
  default     = 3
}
