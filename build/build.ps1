#!/usr/bin/env pwsh
<#
.SYNOPSIS
    TSUNAMI multi-platform build script.

.DESCRIPTION
    Builds tsunami-server and tsunami-client for all supported platforms,
    organized by version into the build/ directory.

.PARAMETER Version
    Semantic version string (e.g., "1.0.0"). Defaults to "dev".

.PARAMETER Platforms
    Comma-separated list of GOOS-GOARCH targets to build.
    Defaults to: linux-amd64,linux-arm64,windows-amd64,darwin-amd64,darwin-arm64

.PARAMETER LDFlags
    Extra ldflags passed to go build. Version info is always injected.

.PARAMETER Clean
    If set, removes the versioned output directory before building.

.PARAMETER SkipChecksum
    If set, skips SHA-256 checksum generation.

.EXAMPLE
    .\build.ps1 -Version 1.2.0
    .\build.ps1 -Version 1.2.0 -Platforms "linux-amd64,windows-amd64"
    .\build.ps1 -Clean -Version 2.0.0-beta
#>

param(
    [string]$Version    = "dev",
    [string]$Platforms  = "linux-amd64,linux-arm64,windows-amd64,darwin-amd64,darwin-arm64",
    [string]$LDFlags    = "",
    [switch]$Clean,
    [switch]$SkipChecksum
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

# -- Paths -----------------------------------------------------------------
$ScriptDir  = Split-Path -Parent $MyInvocation.MyCommand.Definition
$ProjectDir = Split-Path -Parent $ScriptDir
$Module     = "github.com/tsunami-protocol/tsunami"
$Commands   = @("tsunami-server", "tsunami-client")

$VersionTag = "v$Version"
$OutDir     = Join-Path $ScriptDir $VersionTag

# -- Banner ----------------------------------------------------------------
Write-Host ""
Write-Host "=====================================================" -ForegroundColor Cyan
Write-Host "           TSUNAMI  Build  System                     " -ForegroundColor Cyan
Write-Host "=====================================================" -ForegroundColor Cyan
Write-Host ""
Write-Host "  Version   : $VersionTag"                              -ForegroundColor White
Write-Host "  Module    : $Module"                                   -ForegroundColor White
Write-Host "  Platforms : $Platforms"                                 -ForegroundColor White
Write-Host "  Output    : $OutDir"                                   -ForegroundColor White
Write-Host ""

# -- Pre-flight ------------------------------------------------------------
if (-not (Get-Command "go" -ErrorAction SilentlyContinue)) {
    Write-Host "ERROR: 'go' not found in PATH." -ForegroundColor Red
    exit 1
}
$GoVersion = (go version) -replace 'go version ',''
Write-Host "  Go        : $GoVersion" -ForegroundColor DarkGray
Write-Host ""

# -- Clean -----------------------------------------------------------------
if ($Clean -and (Test-Path $OutDir)) {
    Write-Host "[clean] Removing $OutDir ..." -ForegroundColor Yellow
    Remove-Item -Recurse -Force $OutDir
}

# -- Build matrix ----------------------------------------------------------
$BuildTime  = (Get-Date -Format "yyyy-MM-ddTHH:mm:sszzz")
$GitCommit  = ""
try { $GitCommit = (git -C $ProjectDir rev-parse --short HEAD 2>$null) } catch {}
if ([string]::IsNullOrEmpty($GitCommit)) { $GitCommit = "unknown" }

$BaseLDFlags = "-s -w -X main.version=$VersionTag -X main.commit=$GitCommit -X main.buildTime=$BuildTime"
if ($LDFlags) { $BaseLDFlags = "$BaseLDFlags $LDFlags" }

$TargetList = $Platforms -split ','
$TotalBuilds = $TargetList.Count * $Commands.Count
$Current = 0
$Failed  = @()

foreach ($target in $TargetList) {
    $parts = $target.Trim() -split '-'
    if ($parts.Count -ne 2) {
        Write-Host "[skip] Invalid target format: '$target' (expected GOOS-GOARCH)" -ForegroundColor Yellow
        continue
    }
    $goos   = $parts[0]
    $goarch = $parts[1]

    $PlatformDir = Join-Path $OutDir "$goos-$goarch"
    if (-not (Test-Path $PlatformDir)) {
        New-Item -ItemType Directory -Path $PlatformDir -Force | Out-Null
    }

    foreach ($cmd in $Commands) {
        $Current++
        $ext = if ($goos -eq "windows") { ".exe" } else { "" }
        $outFile = Join-Path $PlatformDir "$cmd$ext"
        $srcPath = "$Module/cmd/$cmd"

        Write-Host "[$Current/$TotalBuilds] Building $cmd ($goos/$goarch) ..." -ForegroundColor Green -NoNewline

        $env:GOOS   = $goos
        $env:GOARCH  = $goarch
        $env:CGO_ENABLED = "0"

        try {
            $buildArgs = @(
                "build",
                "-trimpath",
                "-ldflags", $BaseLDFlags,
                "-o", $outFile,
                $srcPath
            )
            & go @buildArgs 2>&1 | ForEach-Object { Write-Host "  $_" -ForegroundColor DarkGray }
            if ($LASTEXITCODE -ne 0) { throw "go build exited with code $LASTEXITCODE" }

            $size = (Get-Item $outFile).Length
            $sizeMB = [math]::Round($size / 1MB, 2)
            Write-Host (" OK ({0} MB)" -f $sizeMB) -ForegroundColor Green
        }
        catch {
            Write-Host " FAILED" -ForegroundColor Red
            Write-Host ("  Error: {0}" -f $_) -ForegroundColor Red
            $Failed += "$cmd ($goos/$goarch)"
        }
        finally {
            Remove-Item Env:\GOOS   -ErrorAction SilentlyContinue
            Remove-Item Env:\GOARCH  -ErrorAction SilentlyContinue
            Remove-Item Env:\CGO_ENABLED -ErrorAction SilentlyContinue
        }
    }
}

# -- Checksums -------------------------------------------------------------
if (-not $SkipChecksum -and (Test-Path $OutDir)) {
    Write-Host ""
    Write-Host "[checksum] Generating SHA-256 checksums ..." -ForegroundColor Cyan

    $ChecksumFile = Join-Path $OutDir "checksums.sha256"
    $checksums = @()

    Get-ChildItem -Path $OutDir -Recurse -File |
        Where-Object { $_.Name -ne "checksums.sha256" } |
        ForEach-Object {
            $hash = (Get-FileHash -Path $_.FullName -Algorithm SHA256).Hash.ToLower()
            $rel  = $_.FullName.Substring($OutDir.Length + 1) -replace '\\','/'
            $checksums += "$hash  $rel"
        }

    $checksums | Out-File -FilePath $ChecksumFile -Encoding utf8

    Write-Host "  Saved to: $ChecksumFile" -ForegroundColor DarkGray
}

# -- Summary ---------------------------------------------------------------
Write-Host ""
Write-Host "=====================================================" -ForegroundColor Cyan

if ($Failed.Count -gt 0) {
    Write-Host ("  Build completed with {0} failure(s):" -f $Failed.Count) -ForegroundColor Yellow
    foreach ($f in $Failed) {
        Write-Host "    x $f" -ForegroundColor Red
    }
    exit 1
} else {
    Write-Host "  [OK] All $TotalBuilds build(s) succeeded." -ForegroundColor Green
    Write-Host "  Output: $OutDir" -ForegroundColor White
}
Write-Host "=====================================================" -ForegroundColor Cyan
Write-Host ""
