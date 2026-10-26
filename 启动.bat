@echo off
chcp 65001 >nul
cd /d "%~dp0"
title selfnas

if not exist "%~dp0selfnas.exe" (
  echo 找不到 selfnas.exe
  echo 先编译： go build -o selfnas.exe .
  pause
  exit /b 1
)

echo 正在启动 selfnas...
start "selfnas" /min "%~dp0selfnas.exe"
timeout /t 2 >nul
echo.
echo   端口和根目录都在 config.json 里，首次启动会自动生成。
echo   具体访问地址看 selfnas.exe 窗口的横幅，或 logs 下的日志。
echo   关闭服务：结束 selfnas.exe 进程。
echo.
pause
