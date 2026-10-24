@echo off
setlocal
cd /d "%~dp0"

if not exist "%~dp0.gotmp" mkdir "%~dp0.gotmp"
set GOTMPDIR=%~dp0.gotmp

if not exist ".env" (
  echo .env not found. Copy .env.example to .env and fill in the database settings.
  exit /b 1
)

rem Creates the database if missing, then applies all SQL migrations.
go run ./cmd/migrate
