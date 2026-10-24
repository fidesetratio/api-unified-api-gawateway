@echo off
setlocal
cd /d "%~dp0"

rem Build/test executables go in .gotmp inside the project instead of %TEMP%,
rem because Application Control may block running unsigned .exe files from %TEMP%.
rem Prefer C:\tools\go\gotmp when the Go folder exists (it ran when other folders were blocked).
if exist "C:\tools\go" (
  if not exist "C:\tools\go\gotmp" mkdir "C:\tools\go\gotmp"
  set GOTMPDIR=C:\tools\go\gotmp
) else (
  if not exist "%~dp0.gotmp" mkdir "%~dp0.gotmp"
  set GOTMPDIR=%~dp0.gotmp
)

rem Database tests run only when GATEWAY_TEST_DATABASE_URL is set, e.g.
rem   set GATEWAY_TEST_DATABASE_URL=postgres://postgres:PASSWORD@127.0.0.1:5432/gateway_test?sslmode=disable
rem (write @ inside the password as %%40). Each test uses a throwaway schema.
if not defined GATEWAY_TEST_DATABASE_URL echo NOTE: GATEWAY_TEST_DATABASE_URL is not set, database tests will be skipped.

rem Usage: build.bat [test|build|all]   (default: all)
set TARGET=%1
if "%TARGET%"=="" set TARGET=all

if /i "%TARGET%"=="build" goto build

echo === go vet ===
go vet ./...
if errorlevel 1 goto fail

echo === go test ===
go test -count=1 ./...
if errorlevel 1 goto fail

if /i "%TARGET%"=="test" goto done

:build
echo === go build ===
if not exist bin mkdir bin
go build -o bin\gateway.exe .\cmd\gateway
if errorlevel 1 goto fail
go build -o bin\gatewayctl.exe .\cmd\gatewayctl
if errorlevel 1 goto fail
go build -o bin\migrate.exe .\cmd\migrate
if errorlevel 1 goto fail
bin\migrate.exe -sql > schema.sql
if errorlevel 1 goto fail
echo Built bin\gateway.exe, bin\gatewayctl.exe, bin\migrate.exe and schema.sql

:done
echo.
echo OK
exit /b 0

:fail
echo.
echo FAILED
exit /b 1
