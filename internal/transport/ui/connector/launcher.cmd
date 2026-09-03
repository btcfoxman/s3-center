@echo off
setlocal
title S3 Center Cloudflare Connector 2.0
cd /d "%~dp0"
node connector.mjs "%~1"
if errorlevel 1 (
  echo.
  echo S3 Center Connector failed. Review the message above, then press any key to close.
  pause >nul
)
