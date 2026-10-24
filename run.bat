@echo off
setlocal
cd /d "%~dp0"

rem Go's temp executables live in the project (see build.bat for why).
if not exist "%~dp0.gotmp" mkdir "%~dp0.gotmp"
set GOTMPDIR=%~dp0.gotmp

rem Configuration comes from .env (DB_HOST, DB_PORT, DB_USER, DB_PASSWORD, DB_NAME).
rem The database, tables and master.key are created on first start.
if not exist ".env" (
  echo .env not found. Copy .env.example to .env and fill in the database settings.
  exit /b 1
)

echo Starting gateway on http://localhost:8080  ^(health: /healthz^)
go run ./cmd/gateway
