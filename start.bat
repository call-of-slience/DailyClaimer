@echo off
chcp 65001 >nul
title ChinaCPA 代签器
cd /d "%~dp0"

echo.
echo  ============================================
echo   ChinaCPA 代签器
echo  ============================================

rem ── 1. 先清掉可能还在跑的旧实例 ─────────────────────────
rem  同一个端口只能起一个实例。如果旧实例还活着，新进程会静默启动失败，
rem  浏览器连到的其实还是旧代码 —— 表现为「改了没反应 / 页面 404」。
taskkill /f /im chinacpa.exe >nul 2>&1
if not errorlevel 1 (
  echo  [1/3] 已关闭正在运行的旧实例
) else (
  echo  [1/3] 没有旧实例在运行
)
rem 等端口真正释放（taskkill 是异步的）
ping -n 2 127.0.0.1 >nul

rem ── 2. 每次都重新编译 ──────────────────────────────────
rem  以前是「exe 不存在才编译」，结果改了代码后一直跑旧二进制。
where go >nul 2>&1
if errorlevel 1 goto nogo
rem  发行包里没有 cmd\ 源码目录：跳过编译，直接跑 exe
if not exist cmd\chinacpa goto nogo

echo  [2/3] 正在编译最新代码...
go build -o chinacpa.exe ./cmd/chinacpa
if errorlevel 1 (
  echo.
  echo  [错误] 编译失败，请看上面的编译信息。
  pause
  exit /b 1
)
goto built

:nogo
if not exist chinacpa.exe (
  echo.
  echo  [错误] 没找到 go 命令，也没有已编译的 chinacpa.exe。
  echo         请安装 Go 1.23+ ，或把别人编译好的 chinacpa.exe 放到本目录。
  pause
  exit /b 1
)
rem  发行包只有 exe 没有源码，这里不要再去编译，直接用现成的
echo  [2/3] 直接使用现有 chinacpa.exe（无源码或未装 Go，跳过编译）

:built
echo  [3/3] 启动中，面板地址 http://127.0.0.1:8313/
echo.
echo  关闭本窗口即退出程序。若面板打不开，看本窗口里的报错。
echo.

rem  先起服务，再延迟打开浏览器，避免「连接被拒绝」
start "" cmd /c "ping -n 3 127.0.0.1 >nul & start http://127.0.0.1:8313/"
chinacpa.exe -config config.json

echo.
echo  [ChinaCPA] 已退出（退出码 %errorlevel%）
pause
