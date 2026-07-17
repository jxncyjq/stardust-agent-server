param(
    [Parameter(Mandatory = $true)]
    [string]$Version,
    [string]$Commit = "local",
    [string]$BuildTime = "",
    [string]$OutDir = ".\dist"
)

$ErrorActionPreference = "Stop"

if ([string]::IsNullOrWhiteSpace($BuildTime)) {
    $BuildTime = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
}

$root = Resolve-Path (Join-Path $PSScriptRoot "..")
$out = Join-Path $root $OutDir
New-Item -ItemType Directory -Force -Path $out | Out-Null

$targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Name = "legion-agent-windows-amd64.exe" },
    @{ GOOS = "linux"; GOARCH = "amd64"; Name = "legion-agent-linux-amd64" },
    @{ GOOS = "linux"; GOARCH = "arm64"; Name = "legion-agent-linux-arm64" }
)

$module = "github.com/stardust/legion-agent/internal/version"
$ldflags = "-s -w -X '$module.Version=$Version' -X '$module.Commit=$Commit' -X '$module.BuildTime=$BuildTime'"

foreach ($target in $targets) {
    $env:GOOS = $target.GOOS
    $env:GOARCH = $target.GOARCH
    $artifact = Join-Path $out $target.Name
    go build -p=1 -buildvcs=false -ldflags $ldflags -o $artifact ./cmd/agent
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed for $($target.GOOS)/$($target.GOARCH)"
    }
    Write-Output "artifact=$artifact"
}

Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
