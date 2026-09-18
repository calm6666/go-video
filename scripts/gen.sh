#!/usr/bin/env bash
set -euo pipefail

# macOS/Linux generator. Run from any directory inside the repository.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
service_filter="${1:-}"
module="$(cd "$repo_root" && go list -m)"

generate_service() {
  local service_dir="$1"
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
