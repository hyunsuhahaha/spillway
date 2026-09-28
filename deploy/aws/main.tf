# Spillway — AWS as a second burst provider.
#
#   ALB (public)  ->  ECS Fargate service "spillway-app" (desired 0 until a burst)
#   Each task runs the app plus a Tailscale sidecar in userspace mode; the app
#   reaches the DB router on the GCP anchor VM through the sidecar's SOCKS5
#   proxy, so no database port is exposed to the internet.
#
#   terraform apply -target=aws_ecr_repository.spillway
#   ../../scripts/push-images.sh aws <account>.dkr.ecr.<region>.amazonaws.com/spillway
#   terraform apply
terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = { source = "hashicorp/aws", version = ">= 5.0" }
  }
}

provider "aws" {
  region = var.region
}

data "aws_vpc" "default" {
  default = true
}

data "aws_subnets" "default" {
  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.default.id]
  }
  filter {
    name   = "default-for-az"
    values = ["true"]
  }
}

locals {
  app_image = var.app_image != "" ? var.app_image : "${aws_ecr_repository.spillway.repository_url}:latest"
}

resource "aws_ecr_repository" "spillway" {
  name         = "spillway"
  force_delete = true
}

# ------------------------------------------------------------------ network

resource "aws_security_group" "alb" {
  name   = "spillway-alb"
  vpc_id = data.aws_vpc.default.id
  ingress {
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group" "task" {
  name   = "spillway-task"
  vpc_id = data.aws_vpc.default.id
  ingress {
    from_port       = 8080
    to_port         = 8080
    protocol        = "tcp"
    security_groups = [aws_security_group.alb.id]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_lb" "spillway" {
  name               = "spillway-alb"
  load_balancer_type = "application"
  security_groups    = [aws_security_group.alb.id]
  subnets            = data.aws_subnets.default.ids
}

resource "aws_lb_target_group" "app" {
  name                 = "spillway-app"
  port                 = 8080
  protocol             = "HTTP"
  target_type          = "ip"
  vpc_id               = data.aws_vpc.default.id
  deregistration_delay = 5
  health_check {
    path                = "/healthz"
    interval            = 5
    timeout             = 3
    healthy_threshold   = 2
    unhealthy_threshold = 2
  }
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.spillway.arn
  port              = 80
  protocol          = "HTTP"
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.app.arn
  }
}

# ------------------------------------------------------------------ ECS

resource "aws_ecs_cluster" "spillway" {
  name = "spillway"
}

resource "aws_cloudwatch_log_group" "app" {
  name              = "/ecs/spillway"
  retention_in_days = 3
}

resource "aws_iam_role" "exec" {
  name = "spillway-task-exec"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "ecs-tasks.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}

resource "aws_iam_role_policy_attachment" "exec" {
  role       = aws_iam_role.exec.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_ecs_task_definition" "app" {
  family                   = "spillway-app"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 512
  memory                   = 1024
  execution_role_arn       = aws_iam_role.exec.arn

  container_definitions = jsonencode([
    {
      name      = "tailscale"
      image     = "tailscale/tailscale:stable"
      essential = true
      environment = [
        { name = "TS_AUTHKEY", value = var.tailscale_authkey },
        { name = "TS_USERSPACE", value = "true" },
        { name = "TS_SOCKS5_SERVER", value = "localhost:1055" },
        { name = "TS_HOSTNAME", value = "spillway-aws" },
        { name = "TS_ACCEPT_DNS", value = "false" },
      ]
      logConfiguration = {
        logDriver = "awslogs"
        options   = { awslogs-group = aws_cloudwatch_log_group.app.name, awslogs-region = var.region, awslogs-stream-prefix = "tailscale" }
      }
    },
    {
      name         = "app"
      image        = local.app_image
      essential    = true
      portMappings = [{ containerPort = 8080, protocol = "tcp" }]
      environment = [
        { name = "PORT", value = "8080" },
        { name = "SITE", value = "cloud-aws" },
        { name = "DB_URL", value = "postgres://spillway:${var.app_db_password}@${var.cloud_ts_ip}:6432/spillway?sslmode=disable" },
        { name = "DB_SOCKS5", value = "localhost:1055" },
        { name = "WORK_MS", value = tostring(var.work_ms) },
        { name = "MAX_INFLIGHT", value = tostring(var.max_inflight) },
      ]
      logConfiguration = {
        logDriver = "awslogs"
        options   = { awslogs-group = aws_cloudwatch_log_group.app.name, awslogs-region = var.region, awslogs-stream-prefix = "app" }
      }
    },
  ])
}

resource "aws_ecs_service" "app" {
  name            = "spillway-app"
  cluster         = aws_ecs_cluster.spillway.id
  task_definition = aws_ecs_task_definition.app.arn
  desired_count   = 0
  launch_type     = "FARGATE"

  network_configuration {
    subnets          = data.aws_subnets.default.ids
    security_groups  = [aws_security_group.task.id]
    assign_public_ip = true # pull images without a NAT gateway
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.app.arn
    container_name   = "app"
    container_port   = 8080
  }

  # The Spillway control plane owns desired_count at runtime.
  lifecycle {
    ignore_changes = [desired_count]
  }

  depends_on = [aws_lb_listener.http]
}

# ------------------------------------------------------------------ control-plane credentials

resource "aws_iam_user" "control" {
  name = "spillway-control"
}

resource "aws_iam_user_policy" "control" {
  name = "spillway-scale-ecs"
  user = aws_iam_user.control.name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["ecs:UpdateService", "ecs:DescribeServices"]
      Resource = aws_ecs_service.app.id
    }]
  })
}

resource "aws_iam_access_key" "control" {
  user = aws_iam_user.control.name
}
