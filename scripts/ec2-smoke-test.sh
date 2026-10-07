#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUNTIME_DIR="$ROOT_DIR/.tmp/ec2-smoke"
KEY_PATH="$RUNTIME_DIR/id_ed25519"
METADATA_PATH="$RUNTIME_DIR/metadata.env"
SSH_USER="${SMOKE_SSH_USER:-ubuntu}"
: "${GOCACHE:=/tmp/bridgectl-go-build}"
: "${GOFLAGS:=-buildvcs=false}"

AWS_REGION="${SMOKE_AWS_REGION:-${AWS_REGION:-}}"
INSTANCE_TYPE="${SMOKE_INSTANCE_TYPE:-t3.small}"
APT_SUITE="${SMOKE_APT_SUITE:-noble}"
REPO_BASE_URL="${SMOKE_REPO_BASE_URL:-https://orchael.github.io/bridgectl/apt}"
NAME_PREFIX="${SMOKE_NAME_PREFIX:-aab-apt-smoke}"
ACTION="run"

export GOCACHE
export GOFLAGS

usage() {
  cat <<EOF
Usage: $(basename "$0") [run|destroy|status] [options]

Options:
  --region <region>           AWS region to use
  --instance-type <type>      EC2 instance type (default: $INSTANCE_TYPE)
  --apt-suite <suite>         Ubuntu suite to install from apt (default: $APT_SUITE)
  --repo-base-url <url>       Apt repository base URL
  --name-prefix <prefix>      Name prefix for smoke resources
  -h, --help                  Show this help
EOF
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "ec2-smoke-test: missing required command: $1" >&2
    exit 1
  }
}

load_metadata() {
  if [[ -f "$METADATA_PATH" ]]; then
    # shellcheck disable=SC1090
    source "$METADATA_PATH"
  fi
}

save_metadata() {
  mkdir -p "$RUNTIME_DIR"
  cat >"$METADATA_PATH" <<EOF
AWS_REGION=$AWS_REGION
INSTANCE_ID=$INSTANCE_ID
SECURITY_GROUP_ID=$SECURITY_GROUP_ID
KEY_NAME=$KEY_NAME
PUBLIC_IP=$PUBLIC_IP
APT_SUITE=$APT_SUITE
REPO_BASE_URL=$REPO_BASE_URL
EOF
}

parse_args() {
  if [[ $# -gt 0 && "$1" != --* ]]; then
    ACTION="$1"
    shift
  fi

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --region)
        AWS_REGION="$2"
        shift 2
        ;;
      --instance-type)
        INSTANCE_TYPE="$2"
        shift 2
        ;;
      --apt-suite)
        APT_SUITE="$2"
        shift 2
        ;;
      --repo-base-url)
        REPO_BASE_URL="$2"
        shift 2
        ;;
      --name-prefix)
        NAME_PREFIX="$2"
        shift 2
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        echo "ec2-smoke-test: unknown argument: $1" >&2
        usage >&2
        exit 1
        ;;
    esac
  done
}

require_region() {
  if [[ -z "$AWS_REGION" ]]; then
    echo "ec2-smoke-test: --region is required" >&2
    exit 1
  fi
}

generate_key() {
  mkdir -p "$RUNTIME_DIR"
  chmod 700 "$RUNTIME_DIR"
  if [[ ! -f "$KEY_PATH" ]]; then
    ssh-keygen -t ed25519 -f "$KEY_PATH" -N "" -C "${NAME_PREFIX}@local" >/dev/null
  fi
}

default_vpc_id() {
  aws ec2 describe-vpcs \
    --region "$AWS_REGION" \
    --filters Name=isDefault,Values=true Name=state,Values=available \
    --query 'Vpcs[0].VpcId' \
    --output text
}

default_subnet_id() {
  aws ec2 describe-subnets \
    --region "$AWS_REGION" \
    --filters Name=default-for-az,Values=true \
    --query 'Subnets[0].SubnetId' \
    --output text
}

ami_id() {
  local suite="$1"
  local ssm_path=""
  case "$suite" in
    noble)
      ssm_path="/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id"
      ;;
    plucky)
      ssm_path="/aws/service/canonical/ubuntu/server/25.04/stable/current/amd64/hvm/ebs-gp3/ami-id"
      ;;
    *)
      echo "ec2-smoke-test: unsupported apt suite for EC2 smoke: $suite" >&2
      exit 1
      ;;
  esac

  aws ssm get-parameter \
    --region "$AWS_REGION" \
    --name "$ssm_path" \
    --query 'Parameter.Value' \
    --output text
}

wait_for_ssh() {
  local host="$1"
  for _ in $(seq 1 60); do
    if ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -i "$KEY_PATH" "$SSH_USER@$host" 'echo ok' >/dev/null 2>&1; then
      return
    fi
    sleep 5
  done
  echo "ec2-smoke-test: SSH did not become ready for $host" >&2
  exit 1
}

run_remote_install() {
  scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$KEY_PATH" \
    "$ROOT_DIR/scripts/install.sh" "$SSH_USER@$PUBLIC_IP:/tmp/install.sh" >/dev/null

  ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$KEY_PATH" "$SSH_USER@$PUBLIC_IP" \
    "chmod +x /tmp/install.sh && sudo APT_SUITE='$APT_SUITE' REPO_BASE_URL='$REPO_BASE_URL' /tmp/install.sh && systemctl --user enable --now bridge.service && systemctl --user is-active --quiet bridge.service"
}

run_healthcheck() {
  local tunnel_port="19445"
  local health_bin="$RUNTIME_DIR/plain-healthcheck"
  local tunnel_pid=""

  go build -o "$health_bin" ./e2e/cmd/plain-healthcheck

  ssh -N \
    -L "${tunnel_port}:127.0.0.1:9445" \
    -o ExitOnForwardFailure=yes \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -i "$KEY_PATH" \
    "$SSH_USER@$PUBLIC_IP" &
  tunnel_pid="$!"

  for _ in $(seq 1 30); do
    if "$health_bin" -target "127.0.0.1:${tunnel_port}" >/dev/null 2>&1; then
      [[ -n "$tunnel_pid" ]] && kill "$tunnel_pid" >/dev/null 2>&1 || true
      echo "EC2 APT SMOKE PASSED: host=$PUBLIC_IP suite=$APT_SUITE"
      return
    fi
    sleep 2
  done

  ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$KEY_PATH" "$SSH_USER@$PUBLIC_IP" \
    'sudo journalctl -u bridgectl --no-pager -n 100' >&2 || true
  [[ -n "$tunnel_pid" ]] && kill "$tunnel_pid" >/dev/null 2>&1 || true
  echo "ec2-smoke-test: healthcheck failed" >&2
  exit 1
}

create_stack() {
  local vpc_id subnet_id ingress_cidr image_id

  generate_key
  vpc_id="$(default_vpc_id)"
  subnet_id="$(default_subnet_id)"
  image_id="$(ami_id "$APT_SUITE")"
  ingress_cidr="$(curl -fsSL https://checkip.amazonaws.com | tr -d '\n')/32"

  KEY_NAME="${NAME_PREFIX}-$(date +%s)"
  aws ec2 import-key-pair \
    --region "$AWS_REGION" \
    --key-name "$KEY_NAME" \
    --public-key-material "fileb://${KEY_PATH}.pub" >/dev/null

  SECURITY_GROUP_ID="$(aws ec2 create-security-group \
    --region "$AWS_REGION" \
    --group-name "${KEY_NAME}-sg" \
    --description "Temporary bridgectl apt smoke access" \
    --vpc-id "$vpc_id" \
    --query 'GroupId' \
    --output text)"

  aws ec2 authorize-security-group-ingress \
    --region "$AWS_REGION" \
    --group-id "$SECURITY_GROUP_ID" \
    --ip-permissions "IpProtocol=tcp,FromPort=22,ToPort=22,IpRanges=[{CidrIp=${ingress_cidr},Description=apt-smoke-ssh}]" >/dev/null

  INSTANCE_ID="$(aws ec2 run-instances \
    --region "$AWS_REGION" \
    --image-id "$image_id" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name "$KEY_NAME" \
    --security-group-ids "$SECURITY_GROUP_ID" \
    --subnet-id "$subnet_id" \
    --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=${NAME_PREFIX}}]" \
    --query 'Instances[0].InstanceId' \
    --output text)"

  aws ec2 wait instance-running --region "$AWS_REGION" --instance-ids "$INSTANCE_ID"
  aws ec2 wait instance-status-ok --region "$AWS_REGION" --instance-ids "$INSTANCE_ID"

  PUBLIC_IP="$(aws ec2 describe-instances \
    --region "$AWS_REGION" \
    --instance-ids "$INSTANCE_ID" \
    --query 'Reservations[0].Instances[0].PublicIpAddress' \
    --output text)"

  save_metadata
}

destroy_stack() {
  load_metadata
  require_region

  if [[ -n "${INSTANCE_ID:-}" ]]; then
    aws ec2 terminate-instances --region "$AWS_REGION" --instance-ids "$INSTANCE_ID" >/dev/null || true
    aws ec2 wait instance-terminated --region "$AWS_REGION" --instance-ids "$INSTANCE_ID" || true
  fi

  if [[ -n "${SECURITY_GROUP_ID:-}" ]]; then
    aws ec2 delete-security-group --region "$AWS_REGION" --group-id "$SECURITY_GROUP_ID" >/dev/null || true
  fi

  if [[ -n "${KEY_NAME:-}" ]]; then
    aws ec2 delete-key-pair --region "$AWS_REGION" --key-name "$KEY_NAME" >/dev/null || true
  fi

  rm -f "$METADATA_PATH"
}

status() {
  load_metadata
  if [[ -z "${PUBLIC_IP:-}" ]]; then
    echo "ec2-smoke-test: no saved smoke metadata" >&2
    exit 1
  fi

  cat <<EOF
Region: $AWS_REGION
Instance ID: $INSTANCE_ID
Public IP: $PUBLIC_IP
SSH:
  ssh -i $KEY_PATH $SSH_USER@$PUBLIC_IP
Destroy:
  ./scripts/ec2-smoke-test.sh destroy --region $AWS_REGION
EOF
}

main() {
  require_cmd aws
  require_cmd curl
  require_cmd go
  require_cmd scp
  require_cmd ssh

  parse_args "$@"
  load_metadata

  case "$ACTION" in
    run)
      require_region
      create_stack
      wait_for_ssh "$PUBLIC_IP"
      run_remote_install
      run_healthcheck
      ;;
    destroy)
      destroy_stack
      ;;
    status)
      status
      ;;
    *)
      echo "ec2-smoke-test: unsupported action: $ACTION" >&2
      exit 1
      ;;
  esac
}

main "$@"
