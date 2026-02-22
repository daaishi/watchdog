@echo off
echo Building watchdog.exe ...
go build -ldflags="-s -w" -o dist\watchdog.exe .
if %errorlevel% neq 0 (
    echo Build FAILED.
    pause
    exit /b 1
)

copy /Y config.json dist\config.json >nul
copy /Y README.md dist\README.md >nul

echo.
echo Done! dist\ folder:
dir /B dist\
echo.
pause
