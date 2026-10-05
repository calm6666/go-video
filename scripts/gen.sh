#!/usr/bin/env bash
set -euo pipefail

# macOS/Linux generator. Run from any directory inside the repository.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
service_filter="${1:-}"
module="$(cd "$repo_root" && go list -m)"

# Protobuf's global registry deduplicates descriptors by FILE PATH. A bare file
# name that a dependency already registered makes both packages panic at init
# ("proto: file X is already registered"), and every zrpc server links etcd's
# clientv3 through service discovery. Listed protos are therefore regenerated a
# second time from the repository root, so their descriptor path carries
# directories. Generated files still land beside the .proto and the Go package
# is unchanged. Keep this list in sync with gen.ps1 (gen.cmd only wraps gen.ps1).
descriptor_prefixed_protos=("membership.proto")

regenerate_prefixed_descriptor() {
  local proto_file="$1"
  local relative_proto="${proto_file#"$repo_root"/}"
  echo "protoc (repo-root descriptor path): $relative_proto"
  (
    cd "$repo_root"
    protoc -I . \
      --go_out=. \
      --go-grpc_out=. \
      --go_opt=paths=source_relative \
      --go-grpc_opt=paths=source_relative \
      "$relative_proto"
  )
}

cleanup_zrpc_artifacts() {
  local service_dir="$1"
  local proto_file="$2"
  local has_api="$3"

  if [ "$has_api" = "yes" ]; then
    # goctl api's single entry (<svc>.go) is authoritative; the zrpc duplicate
    # entry and its etc sample would leave two main()s in one package.
    find "$service_dir" -mindepth 1 -maxdepth 1 -type f -name '*.v1.go' -delete
    [ -d "$service_dir/etc" ] && find "$service_dir/etc" -mindepth 1 -maxdepth 1 -type f -name '*.v1.yaml' -delete
  fi

  # Clients use rpc/<svc>_grpc.pb.go New<Service>Client, so the generated
  # <svc>/ client wrapper directory is dead code either way.
  local svc_name
  svc_name="$(sed -n 's/^[[:space:]]*service[[:space:]]\{1,\}\([A-Za-z_][A-Za-z0-9_]*\).*/\1/p' "$proto_file" | head -1)"
  if [ -n "$svc_name" ]; then
    local wrapper
    wrapper="$(printf '%s' "${svc_name:0:1}" | tr '[:upper:]' '[:lower:]')${svc_name:1}"
    [ -d "$service_dir/$wrapper" ] && rm -rf "$service_dir/$wrapper"
  fi
}

generate_service() {
  local service_dir="$1"
  local api_count
  api_count="$(find "$service_dir" -type f -name '*.api' | wc -l | tr -d ' ')"
  local has_api="no"
  [ "$api_count" -gt 0 ] && has_api="yes"

  while IFS= read -r api_file; do
    [ -z "$api_file" ] && continue
    echo "goctl api: $api_file"
    goctl api go -api "$api_file" -dir "$service_dir"
  done < <(find "$service_dir" -type f -name '*.api' -print)

  if [ -d "$service_dir/rpc" ]; then
    while IFS= read -r proto_file; do
      [ -z "$proto_file" ] && continue
      echo "goctl rpc: $proto_file"
      proto_name="$(basename "$proto_file")"
      (
        cd "$service_dir/rpc"
        goctl rpc protoc "$proto_name" \
          --go_out=. \
          --go-grpc_out=. \
          --go_opt=paths=source_relative \
          --go-grpc_opt=paths=source_relative \
          --zrpc_out=.. \
          --module "$module"
      )
      cleanup_zrpc_artifacts "$service_dir" "$proto_file" "$has_api"
      for prefixed in "${descriptor_prefixed_protos[@]}"; do
        if [ "$proto_name" = "$prefixed" ]; then
          regenerate_prefixed_descriptor "$proto_file"
        fi
      done
    done < <(find "$service_dir/rpc" -maxdepth 1 -type f -name '*.proto' -print)
  fi
}

if [ -n "$service_filter" ]; then
  service_dir="$repo_root/services/$service_filter"
  [ -d "$service_dir" ] || { echo "service not found: $service_filter" >&2; exit 1; }
  generate_service "$service_dir"
else
  while IFS= read -r service_dir; do
    generate_service "$service_dir"
  done < <(find "$repo_root/services" -mindepth 1 -maxdepth 1 -type d -print | sort)
fi
