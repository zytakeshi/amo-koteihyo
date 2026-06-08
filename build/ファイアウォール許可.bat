@echo off
chcp 65001 >nul
title AMO工程表 ファイアウォール許可

rem 管理者権限が無ければ、自分自身を昇格(UAC)して再実行する。
net session >nul 2>&1
if %errorlevel% neq 0 (
  echo 管理者の許可が必要です。ポップアップが出たら「はい」を押してください...
  powershell -NoProfile -Command "Start-Process -FilePath '%~f0' -Verb RunAs"
  exit /b
)

set "EXE=%~dp0AMO-koteihyo.exe"
if not exist "%EXE%" (
  echo [エラー] 同じフォルダに AMO-koteihyo.exe が見つかりません。
  echo このバッチと AMO-koteihyo.exe を同じフォルダに置いてから、もう一度実行してください。
  echo.
  pause
  exit /b 1
)

rem 既存の同名ルールを消してから追加（重複防止）。受信を全プロファイルで許可。
netsh advfirewall firewall delete rule name="AMO-koteihyo" >nul 2>&1
netsh advfirewall firewall add rule name="AMO-koteihyo" dir=in action=allow program="%EXE%" enable=yes profile=any

echo.
echo [OK] ファイアウォールの許可を追加しました。
echo      これで iPad / iPhone から接続できます。
echo      このウィンドウは閉じて大丈夫です。
echo.
pause
