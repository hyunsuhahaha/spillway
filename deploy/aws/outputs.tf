output "alb_url" {
  value = "http://${aws_lb.spillway.dns_name}"
}

output "ecr_repository" {
  value = aws_ecr_repository.spillway.repository_url
}

output "cloud_env" {
  description = "Values for deploy/cloud/.env (set CLOUD_PROVIDERS=cloudrun,ecs)"
  sensitive   = true
  value       = <<-EOT
    AWS_REGION=${var.region}
    AWS_ACCESS_KEY_ID=${aws_iam_access_key.control.id}
    AWS_SECRET_ACCESS_KEY=${aws_iam_access_key.control.secret}
    ECS_CLUSTER=${aws_ecs_cluster.spillway.name}
    ECS_SERVICE=${aws_ecs_service.app.name}
    ECS_ENDPOINT_URL=http://${aws_lb.spillway.dns_name}
  EOT
}
