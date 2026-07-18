@echo off
chcp 65001 >nul
setlocal
title AMO工程表 かんたんアップデート

echo ============================================================
echo   AMO BARBER 工程表  かんたんアップデート
echo ============================================================
echo.
echo   新しいバージョンに更新します。
echo   データ（打刻・工程表）は消えません。自動でバックアップします。
echo.
echo   このウィンドウは、終わるまで閉じないでください。
echo.
echo   新しいファイルを確認・ダウンロードしています...
echo   回線により数分かかることがあります。そのままお待ちください...
echo.

rem 日本語トークンは環境変数で PowerShell へ渡す（ファイアウォール許可.bat と同じ方式）。
rem これにより PowerShell 本文は ASCII のみで書ける。
set "AMO_BAT=%~f0"
set "AMO_ALT=AMO工程表.exe"
set "AMO_FRESHDIR=AMO工程表"
set "AMO_ODDESK=デスクトップ"

rem 本体処理は、このバッチ末尾の PowerShell 本文を UTF-8 として読み込んで実行する。
rem マーカーは分割して書き、検索が本文側の一箇所だけに当たるようにする。
powershell -NoProfile -ExecutionPolicy Bypass -Command "$m='#__'+'PSBODY'+'__#'; $t=[System.IO.File]::ReadAllText($env:AMO_BAT,[System.Text.Encoding]::UTF8); $i=$t.IndexOf($m); if($i -lt 0){exit 98}; Invoke-Expression $t.Substring($i)"
set "RC=%errorlevel%"

echo.
rem 注意: powershell.exe -Command は終了時エラーで raw 1 を返すため、1 は成功に使わない。
rem      更新成功=0、フレッシュ成功=31。raw 1 は :err_other（コード表示）へ落とす。
if "%RC%"=="0" goto :ok_update
if "%RC%"=="31" goto :ok_fresh
if "%RC%"=="10" goto :err_hash
if "%RC%"=="11" goto :err_backup
if "%RC%"=="12" goto :err_download
if "%RC%"=="13" goto :err_launch
if "%RC%"=="14" goto :err_compat
if "%RC%"=="21" goto :err_rollback
if "%RC%"=="30" goto :err_hash_fresh
if "%RC%"=="32" goto :err_download_fresh
if "%RC%"=="33" goto :err_install_fresh
if "%RC%"=="40" goto :err_other_fresh
goto :err_other

:ok_update
echo ************************************************************
echo *                                                          *
echo *        更新が完了しました！                              *
echo *                                                          *
echo ************************************************************
echo.
echo   新しいバージョンを、この場所に入れました：
if exist "%TEMP%\amo_update_target.txt" type "%TEMP%\amo_update_target.txt"
echo.
echo   古いバージョンは、同じフォルダに次の名前で残してあります（消していません）：
if exist "%TEMP%\amo_update_backup.txt" type "%TEMP%\amo_update_backup.txt"
echo.
echo   新しいアプリの黒いウィンドウが自動で開きます。
echo   次回からは、アプリの中の「今すぐ更新」ボタンだけで更新できます。
echo.
echo   このウィンドウは閉じて大丈夫です。
echo.
pause
endlocal
exit /b 0

:ok_fresh
echo ************************************************************
echo *                                                          *
echo *        新しくインストールしました！                     *
echo *                                                          *
echo ************************************************************
echo.
echo   以前のアプリが見つからなかったので、デスクトップに
echo   新しく入れました。場所はこちらです：
if exist "%TEMP%\amo_update_target.txt" type "%TEMP%\amo_update_target.txt"
echo.
echo   新しいアプリの黒いウィンドウが自動で開きます。
echo   このウィンドウは閉じて大丈夫です。
echo.
pause
endlocal
exit /b 0

:err_hash
echo [中止] ダウンロードしたファイルの確認に失敗しました。
echo        安全のため、更新を取りやめました。
echo        今までのアプリはそのまま使えます（変更していません）。
echo        少し時間をおいて、もう一度お試しください。
echo        何度も出る場合は、そのままの状態で連絡してください。
echo.
pause
endlocal
exit /b 10

:err_download
echo [中止] インターネットからのダウンロードに失敗しました。
echo        Wi-Fi / インターネットの接続を確認して、
echo        もう一度このファイルをダブルクリックしてください。
echo        今までのアプリはそのまま使えます（変更していません）。
echo.
pause
endlocal
exit /b 12

:err_backup
echo [中止] 今のアプリを入れ替える準備に失敗しました。
echo        アプリがまだ動いている可能性があります。
echo        パソコンを再起動してから、もう一度お試しください。
echo        今までのアプリはそのまま使えます（変更していません）。
echo.
pause
endlocal
exit /b 11

:err_launch
echo [注意] 更新の最終段階で問題が発生しました。
echo        今までのアプリを元に戻して、自動で開き直しました。
echo        いつも通りお使いいただけます（データもそのままです）。
echo        お手数ですが、時間をおいて もう一度お試しください。
echo        何度も起きる場合は、この画面のまま連絡してください。
echo.
pause
endlocal
exit /b 13

:err_compat
echo [注意] 更新は取り消しました。今までのアプリはそのまま使えます。
echo        もう一度お試しください。
echo        （データは無事です。）
echo.
pause
endlocal
exit /b 14

:err_rollback
echo [重要] 更新に失敗し、元のアプリを自動で戻せませんでした。
echo        お手数ですが、下の「左のファイル」の名前を、
echo        矢印の右側の名前に変更すると、元のアプリに戻せます
echo        （データは無事です）：
echo.
if exist "%TEMP%\amo_update_backup.txt" type "%TEMP%\amo_update_backup.txt"
echo.
echo        ※右側の名前のファイルがすでにある場合は、上書きせず
echo          そのままにして連絡してください。
echo        不安なときは、この画面のまま すぐに連絡してください。
echo.
pause
endlocal
exit /b 21

:err_hash_fresh
echo [中止] ダウンロードしたファイルの確認に失敗しました。
echo        安全のため、インストールを中止しました。
echo        少し時間をおいて、もう一度お試しください。
echo        何度も出る場合は、そのままの状態で連絡してください。
echo.
pause
endlocal
exit /b 30

:err_download_fresh
echo [中止] インターネットからのダウンロードに失敗しました。
echo        Wi-Fi / インターネットの接続を確認して、
echo        もう一度このファイルをダブルクリックしてください。
echo.
pause
endlocal
exit /b 32

:err_install_fresh
echo [中止] アプリの設置中に問題が発生しました。
echo        もう一度お試しいただくか、連絡してください。
echo.
pause
endlocal
exit /b 33

:err_other_fresh
echo [中止] 予期しない問題が発生しました（コード %RC%）。
echo        この画面の内容をそのまま連絡してください。
echo.
pause
endlocal
exit /b %RC%

:err_other
echo [中止] 予期しない問題が発生しました（コード %RC%）。
echo        今までのアプリはそのまま残っています。
echo        この画面の内容をそのまま連絡してください。
echo.
pause
endlocal
exit /b %RC%

rem ==========================================================================
rem  以降は PowerShell 本文。cmd は上の exit /b で終了しており、ここは読まない。
rem  bootstrap が UTF-8 として読み込み、マーカー以降を Invoke-Expression する。
rem  本文は ASCII のみ（日本語は $env:AMO_ALT / AMO_FRESHDIR / AMO_ODDESK で受け取る）。
rem ==========================================================================
#__PSBODY__#
# ===== AMO koteihyo one-click upgrader v2 (PowerShell body; read from this .bat as UTF-8) =====
$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'   # IWR progress bar cripples download speed on PS 5.1

# --- constants (Japanese tokens arrive via env vars set by the batch; body stays ASCII) ---
$Primary    = 'AMO-koteihyo.exe'
$Alt        = $env:AMO_ALT                    # alt exe name (Japanese) from batch env
$FreshDir   = $env:AMO_FRESHDIR               # fresh-install folder name (Japanese) from batch env
$OdDesk     = $env:AMO_ODDESK                 # OneDrive Japanese "Desktop" folder name from batch env
$OldSuffix  = '.old-v1.0.0'
$Names      = @($Primary.ToLower(), $Alt.ToLower())
$Exclude    = @('appdata','application data','node_modules','.git','.cache','$recycle.bin','onedrivetemp','windows')
$UrlExe     = 'https://github.com/zytakeshi/amo-koteihyo/releases/latest/download/AMO-koteihyo.exe'
$UrlSha     = $UrlExe + '.sha256'
$Profile2   = $env:USERPROFILE
$fTarget    = Join-Path $env:TEMP 'amo_update_target.txt'
$fBackup    = Join-Path $env:TEMP 'amo_update_backup.txt'

function Write-File([string]$path,[string]$text){
  try { [System.IO.File]::WriteAllText($path, $text, (New-Object System.Text.UTF8Encoding($false))) } catch {}
}
function Remove-Temp($p){
  if($p -and (Test-Path -LiteralPath $p)){ try { Remove-Item -LiteralPath $p -Force -ErrorAction SilentlyContinue } catch {} }
}
function Kill-App(){
  try {
    Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.Path } | Where-Object {
      $Names -contains (Split-Path -Leaf $_.Path).ToLower()
    } | ForEach-Object { try { $_.Kill(); $_.WaitForExit(5000) } catch {} }
  } catch {}
}
function Find-InDir([string]$dir){
  # non-recursive: an install whose folder the updater itself sits in
  $hits = New-Object System.Collections.ArrayList
  if($dir -and (Test-Path -LiteralPath $dir -PathType Container)){
    Get-ChildItem -LiteralPath $dir -File -Force -ErrorAction SilentlyContinue |
      Where-Object { $Names -contains $_.Name.ToLower() } |
      ForEach-Object { [void]$hits.Add($_.FullName) }
  }
  return $hits
}
function Find-Matches([string[]]$roots,[int]$maxDepth){
  # recurse each root, pruning noise dirs at the top level (AppData etc. = the expensive subtrees)
  $hits = New-Object System.Collections.ArrayList
  foreach($root in $roots){
    if([string]::IsNullOrEmpty($root)){ continue }
    if(-not (Test-Path -LiteralPath $root -PathType Container)){ continue }
    Get-ChildItem -LiteralPath $root -Force -ErrorAction SilentlyContinue | ForEach-Object {
      $it = $_
      if($it.PSIsContainer){
        if($Exclude -contains $it.Name.ToLower()){ return }
        Get-ChildItem -LiteralPath $it.FullName -Recurse -Depth $maxDepth -File -Force -ErrorAction SilentlyContinue |
          Where-Object { $Names -contains $_.Name.ToLower() } |
          ForEach-Object { [void]$hits.Add($_.FullName) }
      } elseif($Names -contains $it.Name.ToLower()){
        [void]$hits.Add($it.FullName)
      }
    }
  }
  return $hits
}
function Select-Install($hits){
  if($hits.Count -eq 0){ return $null }
  # prefer an install with a sibling data\koteihyo.json (the real one); tiebreak most-recent
  $withData = @($hits | Where-Object {
    Test-Path -LiteralPath (Join-Path (Split-Path -Parent $_) 'data\koteihyo.json') -PathType Leaf
  })
  $pool = if($withData.Count -gt 0){ $withData } else { @($hits) }
  return ($pool | Sort-Object { (Get-Item -LiteralPath $_ -Force).LastWriteTimeUtc } -Descending | Select-Object -First 1)
}
function Get-KnownDesktop(){
  # KFM/OneDrive-aware real Desktop (handles redirected desktops)
  try {
    $d = [System.Environment]::GetFolderPath([System.Environment+SpecialFolder]::DesktopDirectory)
    if($d -and (Test-Path -LiteralPath $d -PathType Container)){ return $d }
  } catch {}
  return $null
}
function Quarantine([string]$path){
  # move a file out of the way to <path>.failed[.n]; return $true only if the slot is now clear
  try {
    if(-not (Test-Path -LiteralPath $path)){ return $true }
    $q = $path + '.failed'; $k = 2
    while(Test-Path -LiteralPath $q){ $q = $path + '.failed.' + $k; $k++ }
    Move-Item -LiteralPath $path -Destination $q -Force
  } catch { return $false }
  return (-not (Test-Path -LiteralPath $path))
}
function Restore-One($b){
  # success requires: backup moved back AND original present AND backup gone.
  # if the original slot is occupied, the occupier must be quarantined first (and that move verified).
  try {
    $hasBackup = Test-Path -LiteralPath $b.Backup
    if(-not $hasBackup){
      return (Test-Path -LiteralPath $b.Original -PathType Leaf)   # already restored -> ok only if original present
    }
    if(Test-Path -LiteralPath $b.Original){
      if(-not (Quarantine $b.Original)){ return $false }           # occupied slot could not be cleared
    }
    Move-Item -LiteralPath $b.Backup -Destination $b.Original
  } catch { return $false }
  return ((Test-Path -LiteralPath $b.Original -PathType Leaf) -and -not (Test-Path -LiteralPath $b.Backup))
}
function Restore-All($backups){
  $ok = $true
  foreach($b in $backups){ if(-not (Restore-One $b)){ $ok = $false } }
  return $ok
}
function Write-Unresolved($backups){
  # code 21 status: only the STILL-BROKEN items, as "<current-file>  ->  <target-name>" full paths
  $lines = @()
  foreach($b in $backups){
    $resolved = (Test-Path -LiteralPath $b.Original -PathType Leaf) -and (-not (Test-Path -LiteralPath $b.Backup))
    if(-not $resolved){
      $lines += ($b.Backup + '  ->  ' + $b.Original)
    }
  }
  Write-File $fBackup ($lines -join "`r`n")
}
function Fail21($backups){ Write-Unresolved $backups; exit 21 }

$FRESH=$false; $found=$null; $originalPath=$null; $targetDir=$null; $target=$null; $backups=@(); $tmp=$null
Write-File $fTarget ''
Write-File $fBackup ''

try {
  # ---------- 1. LOCATE (script-dir first, then KFM Desktop, then OneDrive/profile, then recursive) ----------
  $scriptDir = $null
  try { $scriptDir = Split-Path -Parent $env:AMO_BAT } catch {}

  $hits = @(Find-InDir $scriptDir)                       # updater sitting inside the install folder wins
  if($hits.Count -eq 0){
    $desktopRoots = @()
    $knownDesktop = Get-KnownDesktop
    if($knownDesktop){ $desktopRoots += $knownDesktop }
    if($env:OneDrive){
      $desktopRoots += (Join-Path $env:OneDrive 'Desktop')
      if($OdDesk){ $desktopRoots += (Join-Path $env:OneDrive $OdDesk) }
    }
    $desktopRoots += (Join-Path $Profile2 'Desktop')
    $desktopRoots += (Join-Path $Profile2 'OneDrive\Desktop')
    if($OdDesk){ $desktopRoots += (Join-Path (Join-Path $Profile2 'OneDrive') $OdDesk) }
    $desktopRoots = @($desktopRoots | Select-Object -Unique)
    $hits = @(Find-Matches $desktopRoots 6)
  }
  if($hits.Count -eq 0){ $hits = @(Find-Matches @($Profile2) 4) }

  if($hits.Count -gt 0){ $found = Select-Install $hits }

  if($found){
    $FRESH = $false
    $originalPath = $found
    $targetDir = Split-Path -Parent $found
  } else {
    $FRESH = $true
    $deskBase = Get-KnownDesktop
    if(-not $deskBase){ $deskBase = Join-Path $Profile2 'Desktop' }
    $targetDir = Join-Path $deskBase $FreshDir
    New-Item -ItemType Directory -Path $targetDir -Force | Out-Null
  }
  $target = Join-Path $targetDir $Primary
  Write-File $fTarget $target

  # ---------- 2. DOWNLOAD both files to temp FIRST (old app still fully intact) ----------
  $tmp = Join-Path $env:TEMP ('AMO-koteihyo.new.' + [System.Guid]::NewGuid().ToString('N') + '.exe')
  try { [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12 } catch {}
  try {
    Invoke-WebRequest -Uri $UrlExe -OutFile $tmp -UseBasicParsing -TimeoutSec 120 -MaximumRedirection 10
    $shaText = (Invoke-WebRequest -Uri $UrlSha -UseBasicParsing -TimeoutSec 120 -MaximumRedirection 10).Content
  } catch {
    Remove-Temp $tmp
    if($FRESH){ exit 32 } else { exit 12 }
  }

  # ---------- 3. VERIFY sha256 (sidecar = bare 64-hex hash); still nothing touched ----------
  $expected = $null
  foreach($tok in ($shaText -split '\s+')){ if($tok){ $expected = $tok.Trim().ToLower(); break } }
  $actual = (Get-FileHash -LiteralPath $tmp -Algorithm SHA256).Hash.ToLower()
  if([string]::IsNullOrEmpty($expected) -or ($actual -ne $expected)){
    Remove-Temp $tmp
    if($FRESH){ exit 30 } else { exit 10 }
  }

  # === verified payload now exists locally; only from here may we disturb the old app ===

  # ---------- 4. KILL the running app ----------
  Kill-App

  # ---------- 5. BACK UP every old exe in the target dir (update mode only), rename never delete ----------
  if(-not $FRESH){
    foreach($nm in @($Primary, $Alt)){
      $op = Join-Path $targetDir $nm
      if(Test-Path -LiteralPath $op -PathType Leaf){
        $bk = Join-Path $targetDir ($nm + $OldSuffix)
        $n = 2
        while(Test-Path -LiteralPath $bk){ $bk = Join-Path $targetDir ($nm + $OldSuffix + '.' + $n); $n++ }
        $moved = $false
        for($i=0; $i -lt 20; $i++){
          try { Move-Item -LiteralPath $op -Destination $bk; $moved = $true; break } catch { Start-Sleep -Milliseconds 500 }
        }
        if(-not $moved){
          $rb = Restore-All $backups      # undo any prior backups
          Remove-Temp $tmp
          if(-not $rb){ Fail21 $backups }  # a prior backup could not be put back -> critical
          exit 11                          # old app fully intact (the failed one was never moved)
        }
        $backups += (New-Object psobject -Property @{ Original = $op; Backup = $bk })
      }
    }
    if($backups.Count -gt 0){
      Write-File $fBackup (($backups | ForEach-Object { Split-Path -Leaf $_.Backup }) -join "`r`n")
    }
  }

  # ---------- 6. SWAP + 7. HEALTH-CHECK LAUNCH (Start-Process -PassThru, reject immediate exit) ----------
  try {
    Move-Item -LiteralPath $tmp -Destination $target       # target slot is empty (canonical was backed up)
    $tmp = $null
    $proc = Start-Process -FilePath $target -WorkingDirectory $targetDir -PassThru
    Start-Sleep -Seconds 3
    if(($proc -eq $null) -or $proc.HasExited){ throw 'new app exited during startup' }
  } catch {
    if(Test-Path -LiteralPath $target){ try { Move-Item -LiteralPath $target -Destination ($target + '.failed') -Force } catch {} }
    Remove-Temp $tmp
    if($FRESH){ exit 33 }                                  # fresh: no old app to restore
    $restored = Restore-All $backups
    if(-not $restored){ Fail21 $backups }                  # CRITICAL: could not restore old app
    if($originalPath){ try { Start-Process -FilePath $originalPath -WorkingDirectory $targetDir } catch {} }
    exit 13
  }

  # ---------- 8. success: compatibility copy under the alt name so an old shortcut keeps working ----------
  # transactional: if the copy fails, the documented "old shortcut keeps working" guarantee is broken,
  # so we abort the whole update rather than exit 0 on a half-kept promise.
  $altExisted = $false
  foreach($b in $backups){ if((Split-Path -Leaf $b.Original).ToLower() -eq $Alt.ToLower()){ $altExisted = $true } }
  if($altExisted){
    $altPath = Join-Path $targetDir $Alt
    $copyOk = $false
    try { Copy-Item -LiteralPath $target -Destination $altPath -Force; $copyOk = $true } catch { $copyOk = $false }
    if(-not $copyOk){
      try { if($proc -and (-not $proc.HasExited)){ $proc.Kill(); $proc.WaitForExit(5000) } } catch {}
      Kill-App
      [void](Quarantine $target)                           # quarantine both new-name files
      [void](Quarantine $altPath)
      $restored = Restore-All $backups
      if(-not $restored){ Fail21 $backups }
      if($originalPath){ try { Start-Process -FilePath $originalPath -WorkingDirectory $targetDir } catch {} }
      exit 14                                               # update cancelled cleanly; old app running
    }
  }

  Write-File $fTarget $target
  if($FRESH){ exit 31 } else { exit 0 }                     # 31 (not 1): raw 1 == PS terminating-error code
}
catch {
  # unexpected error: if we had already backed up (but presumably not completed swap), restore
  try {
    if($backups -and $backups.Count -gt 0){
      if($target -and (Test-Path -LiteralPath $target)){ try { Move-Item -LiteralPath $target -Destination ($target + '.failed') -Force } catch {} }
      $restored = Restore-All $backups
      if(-not $restored){ Remove-Temp $tmp; Fail21 $backups }
      if($originalPath){ try { Start-Process -FilePath $originalPath -WorkingDirectory $targetDir } catch {} }
    }
  } catch {}
  Remove-Temp $tmp
  if($FRESH){ exit 40 } else { exit 20 }
}
