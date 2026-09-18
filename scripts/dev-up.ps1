param(
    [switch]$Down
)

$ErrorActionPreference = "Stop"
$composeFile = Join-Path $PSScriptRoot "..\deploy\docker-compose\docker-compose.yml"
if ($Down) {
    docker compose -f $composeFile down
} else {
    docker compose -f $composeFile up -d
}
