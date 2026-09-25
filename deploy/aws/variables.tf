variable "region" {
  description = "AWS region (Seoul by default)"
  type        = string
  default     = "ap-northeast-2"
}

variable "app_image" {
  description = "App image (defaults to the ECR repository created here, tag latest)"
  type        = string
  default     = ""
}

variable "app_db_password" {
  description = "APP_DB_PASSWORD shared with the other sites"
  type        = string
  sensitive   = true
}

variable "cloud_ts_ip" {
  description = "Tailscale IP of the GCP anchor VM (the DB router listens on :6432 there)"
  type        = string
}

variable "tailscale_authkey" {
  description = "Reusable, ephemeral Tailscale auth key for Fargate tasks"
  type        = string
  sensitive   = true
}

variable "work_ms" {
  type    = number
  default = 25
}

variable "max_inflight" {
  type    = number
  default = 3
}
