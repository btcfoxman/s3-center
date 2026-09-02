$ErrorActionPreference = 'Stop'

$ProtocolRoot = 'HKCU:\Software\Classes\s3center'
if (Test-Path -LiteralPath $ProtocolRoot) {
    Remove-Item -LiteralPath $ProtocolRoot -Recurse -Force
}

Write-Host 'The s3center:// protocol registration was removed.' -ForegroundColor Green
Write-Host 'Browser profiles were retained under %LOCALAPPDATA%\S3Center\Profiles so account sessions are not lost.'
Write-Host 'You may remove %LOCALAPPDATA%\S3Center manually after confirming no profile is needed.'
