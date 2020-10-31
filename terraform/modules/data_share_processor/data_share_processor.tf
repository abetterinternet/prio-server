variable "data_share_processor_name" {
  type = string
}

variable "environment" {
  type = string
}

variable "gcp_project" {
  type = string
}

variable "gcp_region" {
  type = string
}

variable "kubernetes_namespace" {
  type = string
}

variable "packet_decryption_key_kubernetes_secret" {
  type = string
}

variable "certificate_domain" {
  type = string
}

variable "ingestor_manifest_base_url" {
  type = string
}

variable "ingestor_aws_role_arn" {
  type = string
}

variable "ingestor_gcp_service_account_id" {
  type = string
}

variable "peer_share_processor_aws_account_id" {
  type = string
}

variable "peer_share_processor_manifest_base_url" {
  type = string
}

variable "own_manifest_base_url" {
  type = string
}

variable "sum_part_bucket_service_account_email" {
  type = string
}

variable "portal_server_manifest_base_url" {
  type = string
}

locals {
  resource_prefix = "prio-${var.environment}-${var.data_share_processor_name}"
  ingestion_bucket_writer_role_arn = var.ingestor_gcp_service_account_id != "" ? (
    aws_iam_role.ingestor_bucket_writer_role[0].arn
    ) : (
    var.ingestor_aws_role_arn
  )
  ingestion_bucket_name       = "${local.resource_prefix}-ingestion"
  peer_validation_bucket_name = "${local.resource_prefix}-peer-validation"
}

data "aws_caller_identity" "current" {}

# This is the role in AWS we use to construct policy on the S3 buckets. It is
# configured to allow access to the GCP service account for this data share
# processor via Web Identity Federation
# https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_providers_oidc.html
resource "aws_iam_role" "bucket_role" {
  name = "${local.resource_prefix}-bucket-role"
  # We currently use a single role per facilitator to gate read/write access to
  # all buckets. We could define more GCP service accounts and corresponding AWS
  # IAM roles for read/write on each of the ingestion, validation and sum part
  # buckets.
  # Since azp is set in the auth token Google generates, we must check oaud in
  # the role assumption policy, and the value must match what we request when
  # requesting tokens from the GKE metadata service in
  # S3Transport::new_with_client
  # https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_iam-condition-keys.html
  assume_role_policy = <<ROLE
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "accounts.google.com"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "accounts.google.com:sub": "${module.kubernetes.service_account_unique_id}",
          "accounts.google.com:oaud": "sts.amazonaws.com/${data.aws_caller_identity.current.account_id}"
        }
      }
    }
  ]
}
ROLE

  tags = {
    environment = "prio-${var.environment}"
  }
}

# If the ingestor authenticates using a GCP service account, this is the role in
# AWS that their service account assumes. Note the "count" parameter in the
# block, which seems to be the Terraform convention to conditionally create
# resources (c.f. lots of StackOverflow questions and GitHub issues).
resource "aws_iam_role" "ingestor_bucket_writer_role" {
  count              = var.ingestor_gcp_service_account_id != "" ? 1 : 0
  name               = "${local.resource_prefix}-bucket-writer"
  assume_role_policy = <<ROLE
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "accounts.google.com"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "accounts.google.com:sub": "${var.ingestor_gcp_service_account_id}"
        }
      }
    }
  ]
}
ROLE

  tags = {
    environment = "prio-${var.environment}"
  }
}

# The ingestion bucket for this data share processor. This one is different from
# the other two in that we must grant write access to the ingestor but no access
# to the peer share processor.
resource "aws_s3_bucket" "ingestion_bucket" {
  bucket = local.ingestion_bucket_name
  # Force deletion of bucket contents on bucket destroy.
  force_destroy = true
  policy        = <<POLICY
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "AWS": "${local.ingestion_bucket_writer_role_arn}"
      },
      "Action": [
        "s3:AbortMultipartUpload",
        "s3:PutObject",
        "s3:ListMultipartUploadParts",
        "s3:ListBucketMultipartUploads"
      ],
      "Resource": [
        "arn:aws:s3:::${local.ingestion_bucket_name}/*",
        "arn:aws:s3:::${local.ingestion_bucket_name}"
      ]
    },
    {
      "Effect": "Allow",
      "Principal": {
        "AWS": "${aws_iam_role.bucket_role.arn}"
      },
      "Action": [
        "s3:GetObject",
        "s3:ListBucket"
      ],
      "Resource": [
        "arn:aws:s3:::${local.ingestion_bucket_name}/*",
        "arn:aws:s3:::${local.ingestion_bucket_name}"
      ]
    }
  ]
}
POLICY

  tags = {
    environment = var.environment
  }
}

# The peer validation bucket for this data share processor, configured to permit
# the peer share processor to write to it. The policy grants write permissions
# to any role or user in the peer data share processor's AWS account because
# Amazon S3 won't let you define policies in terms of roles that don't exist. In
# any case, since we have no control over or insight into the role assumption
# policies in the other account, we gain nothing by specifying anything beyond
# the AWS account.
# For the aws_iam_role.bucket_role.arn section (the second entry), we temporarily
# allow the various write permissions, so that sample_maker can write sample data.
resource "aws_s3_bucket" "peer_validation_bucket" {
  bucket = local.peer_validation_bucket_name
  # Force deletion of bucket contents on bucket destroy.
  force_destroy = true
  policy        = <<POLICY
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "AWS": "${var.peer_share_processor_aws_account_id}"
      },
      "Action": [
        "s3:AbortMultipartUpload",
        "s3:PutObject",
        "s3:ListMultipartUploadParts",
        "s3:ListBucketMultipartUploads"
      ],
      "Resource": [
        "arn:aws:s3:::${local.peer_validation_bucket_name}/*",
        "arn:aws:s3:::${local.peer_validation_bucket_name}"
      ]
    },
    {
      "Effect": "Allow",
      "Principal": {
        "AWS": "${aws_iam_role.bucket_role.arn}"
      },
      "Action": [
        "s3:GetObject",
        "s3:ListBucket",
        "s3:AbortMultipartUpload",
        "s3:PutObject",
        "s3:ListMultipartUploadParts",
        "s3:ListBucketMultipartUploads"
      ],
      "Resource": [
        "arn:aws:s3:::${local.peer_validation_bucket_name}/*",
        "arn:aws:s3:::${local.peer_validation_bucket_name}"
      ]
    }
  ]
}
POLICY

  tags = {
    environment = var.environment
  }
}

# Besides the validation bucket owned by the peer data share processor, we write
# validation batches into a bucket we control so that we can be certain they
# be available when we perform the aggregation step.
resource "google_storage_bucket" "own_validation_bucket" {
  provider = google-beta
  name     = "${local.resource_prefix}-own-validation"
  location = var.gcp_region
  # Force deletion of bucket contents on bucket destroy. Bucket contents would
  # be re-created by a subsequent deploy so no reason to keep them around.
  force_destroy               = true
  uniform_bucket_level_access = true
}

# Permit the workflow manager and facilitator service account to manage the
# bucket
resource "google_storage_bucket_iam_binding" "own_validation_bucket_admin" {
  bucket = google_storage_bucket.own_validation_bucket.name
  role   = "roles/storage.legacyBucketWriter"
  members = [
    module.kubernetes.service_account_email
  ]
}

module "kubernetes" {
  source                                  = "../../modules/kubernetes/"
  data_share_processor_name               = var.data_share_processor_name
  gcp_project                             = var.gcp_project
  environment                             = var.environment
  kubernetes_namespace                    = var.kubernetes_namespace
  ingestion_bucket                        = "${aws_s3_bucket.ingestion_bucket.region}/${aws_s3_bucket.ingestion_bucket.bucket}"
  ingestion_bucket_role                   = aws_iam_role.bucket_role.arn
  ingestor_manifest_base_url              = var.ingestor_manifest_base_url
  packet_decryption_key_kubernetes_secret = var.packet_decryption_key_kubernetes_secret
  peer_manifest_base_url                  = var.peer_share_processor_manifest_base_url
  peer_validation_bucket                  = "${aws_s3_bucket.peer_validation_bucket.region}/${aws_s3_bucket.peer_validation_bucket.bucket}"
  peer_validation_bucket_role             = aws_iam_role.bucket_role.arn
  own_validation_bucket                   = google_storage_bucket.own_validation_bucket.name
  own_manifest_base_url                   = var.own_manifest_base_url
  sum_part_bucket_service_account_email   = var.sum_part_bucket_service_account_email
  portal_server_manifest_base_url         = var.portal_server_manifest_base_url
}

output "data_share_processor_name" {
  value = var.data_share_processor_name
}

output "kubernetes_namespace" {
  value = var.kubernetes_namespace
}

output "certificate_fqdn" {
  value = "${var.kubernetes_namespace}.${var.certificate_domain}"
}

output "service_account_email" {
  value = module.kubernetes.service_account_email
}

output "specific_manifest" {
  value = {
    format                 = 0
    ingestion-bucket       = "${aws_s3_bucket.ingestion_bucket.region}/${aws_s3_bucket.ingestion_bucket.bucket}",
    peer-validation-bucket = "${aws_s3_bucket.peer_validation_bucket.region}/${aws_s3_bucket.peer_validation_bucket.bucket}",
    batch-signing-public-keys = {
      (module.kubernetes.batch_signing_key) = {
        public-key = ""
        expiration = ""
      }
    }
    packet-encryption-certificates = {
      (var.packet_decryption_key_kubernetes_secret) = {
        certificate = ""
      }
    }
  }
}
