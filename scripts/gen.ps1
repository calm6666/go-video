param(
    [string]$Service = ""
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$servicesRoot = Join-Path $repoRoot "services"
$module = (& go list -m).Trim()
if (-not $module) { throw "cannot resolve Go module; run from a valid repository" }

Write-Host "goctl generation is authoritative: API/RPC framework files will be regenerated from .api/.proto sources."
Write-Host "Do not hand-edit generated handler, routes, types, ServiceContext, RPC client/server, or entry files."

# Cleanup-ZrpcArtifacts removes zrpc artifacts that conflict with the
# repo single-entry convention:
#   1. zrpc generates an entry (<svc>.v1.go / etc/<svc>.v1.yaml); when the
#      service also has an .api file, the goctl api single entry (<svc>.go)
#      is authoritative, so the zrpc duplicate entry is removed;
#   2. zrpc also generates a <svc>/ client wrapper directory; clients use
#      rpc/<svc>_grpc.pb.go New<Service>Client instead, so it is removed.
# Generated file imports/aliases are controlled by the proto go_package
# option (without a package name goctl emits compilable rpc. references);
# this script never edits generated file content.
function Cleanup-ZrpcArtifacts {
    param(
        [Parameter(Mandatory = $true)][string]$TargetDir,
        [Parameter(Mandatory = $true)][string]$ProtoFile,
        [Parameter(Mandatory = $true)][bool]$HasApi
    )

    if ($HasApi) {
        Get-ChildItem -LiteralPath $TargetDir -File -Filter '*.v1.go' -ErrorAction SilentlyContinue |
            Remove-Item -Force
        Get-ChildItem -LiteralPath (Join-Path $TargetDir 'etc') -File -Filter '*.v1.yaml' -ErrorAction SilentlyContinue |
            Remove-Item -Force
    }

    $svcLine = Select-String -LiteralPath $ProtoFile -Pattern 'service\s+\w+' | Select-Object -First 1
    if ($svcLine -and $svcLine.Line -match 'service\s+(\w+)') {
        $svcName = $Matches[1]
        $firstChar = $svcName.Substring(0, 1).ToLower()
        $wrapperName = $firstChar + $svcName.Substring(1)
        $wrapperDir = Join-Path $TargetDir $wrapperName
        if (Test-Path -LiteralPath $wrapperDir -PathType Container) {
            Remove-Item -LiteralPath $wrapperDir -Recurse -Force
        }
    }
}

if ($Service) {
    $serviceDir = Join-Path $servicesRoot $Service
    if (-not (Test-Path -LiteralPath $serviceDir -PathType Container)) {
        throw "service not found: $Service"
    }
    $targets = @(Get-Item -LiteralPath $serviceDir)
} else {
    $targets = @(Get-ChildItem -LiteralPath $servicesRoot -Directory)
}

foreach ($target in $targets) {
    $apiFiles = @(Get-ChildItem -LiteralPath $target.FullName -Filter '*.api' -File -Recurse)
    foreach ($api in $apiFiles) {
        Write-Host "goctl api: $($api.FullName)"
        & goctl api go -api $api.FullName -dir $target.FullName
        if ($LASTEXITCODE -ne 0) { throw "goctl api failed for $($api.FullName)" }
    }

    $protoFiles = @(Get-ChildItem -LiteralPath (Join-Path $target.FullName 'rpc') -Filter '*.proto' -File -ErrorAction SilentlyContinue)
    foreach ($proto in $protoFiles) {
        Write-Host "goctl rpc: $($proto.FullName)"
        $rpcDir = Join-Path $target.FullName 'rpc'
        # Run from the rpc source directory and use source-relative protobuf
        # paths. This keeps generated .pb.go files beside the .proto instead
        # of creating a nested module/import path tree under rpc/.
        Push-Location $rpcDir
        try {
            & goctl rpc protoc $proto.Name `
                --go_out=. `
                --go-grpc_out=. `
                --go_opt=paths=source_relative `
                --go-grpc_opt=paths=source_relative `
                --zrpc_out=.. `
                --module $module
            if ($LASTEXITCODE -ne 0) { throw "goctl rpc failed for $($proto.FullName)" }
        } finally {
            Pop-Location
        }
        Cleanup-ZrpcArtifacts -TargetDir $target.FullName -ProtoFile $proto.FullName -HasApi ($apiFiles.Count -gt 0)
    }
}
