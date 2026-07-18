@echo off
chcp 65001 >nul
title AMO工程表 ファイアウォール許可

rem 管理者権限が無ければ、自分自身を昇格(UAC)して再実行し、終了コードを引き継ぐ。
rem パスは環境変数(AMO_SELF)で渡し、-Wait -PassThru で子の ExitCode を伝播する（finding 11）。
net session >nul 2>&1
if %errorlevel% equ 0 goto :admin
echo 管理者の許可が必要です。ポップアップが出たら「はい」を押してください...
set "AMO_SELF=%~f0"
powershell -NoProfile -Command "$p=Start-Process -FilePath $env:AMO_SELF -Verb RunAs -Wait -PassThru; exit $p.ExitCode"
exit /b %errorlevel%

:admin
rem exe パスは環境変数(AMO_FW_EXE)で渡し、単一引用符の文字列へ一切補間しない（アポストロフィ対策）。
set "AMO_FW_EXE=%~dp0AMO-koteihyo.exe"
if not exist "%AMO_FW_EXE%" (
  echo [エラー] 同じフォルダに AMO-koteihyo.exe が見つかりません。
  echo このバッチと AMO-koteihyo.exe を同じフォルダに置いてから、もう一度実行してください。
  echo.
  pause
  exit /b 1
)

rem アプリ本体と「まったく同じ規則名」で許可規則を作る（App 側のライブ検査が
rem そのまま受理できるように）。規則名 = "AMO-koteihyo v2 <hash8>"。
rem hash8 = 小文字に正規化した exe 絶対パスの UTF-8 SHA-256 先頭 8 桁。
rem exe パスは $env:AMO_FW_EXE で読む（スクリプト本文へ補間しない）。
rem netsh は使わず構造化 cmdlet（ロケール非依存）。errorlevel を確認し、成功時だけ [OK]。
powershell -NoProfile -ExecutionPolicy Bypass -Command "$ErrorActionPreference='Stop'; try { $p=[System.IO.Path]::GetFullPath($env:AMO_FW_EXE); try { $rp=(Resolve-Path -LiteralPath $p -ErrorAction Stop).ProviderPath; if ($rp) { $p=$rp } } catch {}; $bytes=[System.Text.Encoding]::UTF8.GetBytes($p.ToLower()); $hash=[System.BitConverter]::ToString([System.Security.Cryptography.SHA256]::Create().ComputeHash($bytes)).Replace('-','').ToLower(); $rule='AMO-koteihyo v2 '+$hash.Substring(0,8); Get-NetFirewallRule -DisplayName $rule -ErrorAction SilentlyContinue | Remove-NetFirewallRule -ErrorAction SilentlyContinue; Get-NetFirewallRule -ErrorAction SilentlyContinue | Where-Object { ($_.DisplayName -eq 'AMO-koteihyo') -or (($_.DisplayName -match '^AMO-koteihyo v2 [0-9a-f]{8}$') -and ($_.DisplayName -ne $rule)) } | Remove-NetFirewallRule -ErrorAction SilentlyContinue; New-NetFirewallRule -DisplayName $rule -Direction Inbound -Action Allow -Enabled True -Profile Any -Protocol TCP -RemoteAddress LocalSubnet -Program $p | Out-Null; exit 0 } catch { Write-Host $_.Exception.Message; exit 1 }"

if %errorlevel% neq 0 (
  echo.
  echo [エラー] ファイアウォールの許可に失敗しました。
  echo         もう一度このバッチを「管理者として実行」で試すか、
  echo         使い方ガイドの「困ったとき」→ ファイアウォールの項目をご覧ください。
  echo.
  pause
  exit /b 1
)

echo.
echo [OK] ファイアウォールの許可を追加しました。
echo      これで iPad / iPhone から接続できます。
echo      このウィンドウは閉じて大丈夫です。
echo.
pause
