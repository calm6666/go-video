param(
    [ValidateSet('status', 'up')]
    [string]$Action = 'status'
)

$ErrorActionPreference = "Stop"
Write-Host "migration action: $Action"
Write-Host "TODO: connect the selected migration runner after the first service schema is implemented."
