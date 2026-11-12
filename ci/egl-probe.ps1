# Empirical probe, run on a real Windows machine (GitHub Actions runner).
#
# Installs the Epic Games Launcher and answers the questions the claimer's
# design depends on:
#   1. Where does the installer put things (registry / files)?
#   2. Does "-cefdebug=<port>" open a Chrome DevTools endpoint?
#   3. Does "-silent" keep the window off the desktop?
#   4. Does the launcher work on a separate, never-displayed desktop?
#   5. Which DevTools commands work on the launcher's embedded browser?
#
# Nothing here signs in to an account.
param(
    [ValidateSet('silent', 'desktop', 'plain')]
    [string]$Test = 'silent',
    [int]$Port = 9222,
    [int]$BootMinutes = 8
)

$ErrorActionPreference = 'Continue'
$ProgressPreference = 'SilentlyContinue'
$OutDir = Join-Path $env:RUNNER_TEMP 'egl-probe'
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

$script:Started = Get-Date
function Section([string]$t) { Write-Host ""; Write-Host ("=================== {0}  [+{1}s] ===================" -f $t, [int]((Get-Date) - $script:Started).TotalSeconds) }

Add-Type -TypeDefinition @'
using System;
using System.Text;
using System.Collections.Generic;
using System.Runtime.InteropServices;

public static class WinProbe {
    public delegate bool EnumProc(IntPtr h, IntPtr l);
    [DllImport("user32.dll")] static extern bool EnumWindows(EnumProc cb, IntPtr l);
    [DllImport("user32.dll")] static extern bool EnumDesktopWindows(IntPtr hDesk, EnumProc cb, IntPtr l);
    [DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
    [DllImport("user32.dll")] static extern bool IsWindowVisible(IntPtr h);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] static extern int GetClassName(IntPtr h, StringBuilder s, int n);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
    [DllImport("user32.dll")] static extern bool GetWindowRect(IntPtr h, out RECT r);
    [DllImport("user32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    public static extern IntPtr CreateDesktopW(string name, IntPtr dev, IntPtr devmode, uint flags, uint access, IntPtr sa);
    [DllImport("user32.dll", SetLastError = true)] public static extern bool CloseDesktop(IntPtr h);
    [DllImport("user32.dll")] static extern bool PostMessageW(IntPtr h, uint msg, IntPtr w, IntPtr l);

    [StructLayout(LayoutKind.Sequential)] public struct RECT { public int L, T, R, B; }

    public static List<string> List(IntPtr desk) {
        var o = new List<string>();
        EnumProc cb = (h, l) => {
            uint pid; GetWindowThreadProcessId(h, out pid);
            var c = new StringBuilder(256); GetClassName(h, c, 256);
            var t = new StringBuilder(256); GetWindowText(h, t, 256);
            RECT r; GetWindowRect(h, out r);
            o.Add(pid + "|" + (IsWindowVisible(h) ? "VISIBLE" : "hidden") + "|" + c + "|" + t + "|" + r.L + "," + r.T + "," + r.R + "," + r.B);
            return true;
        };
        if (desk == IntPtr.Zero) EnumWindows(cb, IntPtr.Zero); else EnumDesktopWindows(desk, cb, IntPtr.Zero);
        GC.KeepAlive(cb);
        return o;
    }

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    struct STARTUPINFO {
        public int cb; public string lpReserved; public string lpDesktop; public string lpTitle;
        public int dwX, dwY, dwXSize, dwYSize, dwXCountChars, dwYCountChars, dwFillAttribute, dwFlags;
        public short wShowWindow, cbReserved2; public IntPtr lpReserved2, hStdInput, hStdOutput, hStdError;
    }
    [StructLayout(LayoutKind.Sequential)] struct PROCESS_INFORMATION { public IntPtr hProcess, hThread; public int dwProcessId, dwThreadId; }
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    static extern bool CreateProcessW(string app, StringBuilder cmd, IntPtr pa, IntPtr ta, bool inherit, uint flags, IntPtr env, string cwd, ref STARTUPINFO si, out PROCESS_INFORMATION pi);

    public static int StartOnDesktop(string desktop, string cmdline, string cwd) {
        var si = new STARTUPINFO(); si.cb = Marshal.SizeOf(typeof(STARTUPINFO)); si.lpDesktop = desktop;
        PROCESS_INFORMATION pi;
        if (!CreateProcessW(null, new StringBuilder(cmdline), IntPtr.Zero, IntPtr.Zero, false, 0, IntPtr.Zero, cwd, ref si, out pi))
            throw new System.ComponentModel.Win32Exception(Marshal.GetLastWin32Error());
        return pi.dwProcessId;
    }
}
'@

function Get-EpicProcs {
    Get-CimInstance Win32_Process |
        Where-Object { $_.Name -match '^(Epic|Unreal|EOS|CrashReport|BuildPatch)' -or $_.ExecutablePath -match 'Epic Games' } |
        Select-Object ProcessId, ParentProcessId, Name, CommandLine
}

function Show-EpicProcs([string]$why) {
    $p = @(Get-EpicProcs)
    Write-Host "[procs:$why] count=$($p.Count)"
    foreach ($x in $p) {
        $cl = "$($x.CommandLine)"
        if ($cl.Length -gt 420) { $cl = $cl.Substring(0, 420) + '...' }
        Write-Host ("  pid={0} ppid={1} {2} :: {3}" -f $x.ProcessId, $x.ParentProcessId, $x.Name, $cl)
    }
}

function Show-Windows([string]$why, [IntPtr]$desk) {
    $pids = @(Get-EpicProcs | ForEach-Object { [string]$_.ProcessId })
    $all = [WinProbe]::List($desk)
    $mine = @($all | Where-Object { $pids -contains ($_ -split '\|')[0] })
    $vis = @($mine | Where-Object { $_ -match '\|VISIBLE\|' })
    Write-Host "[windows:$why] total-on-desktop=$($all.Count) launcher-owned=$($mine.Count) launcher-VISIBLE=$($vis.Count)"
    foreach ($w in ($mine | Select-Object -First 40)) { Write-Host "  $w" }
}

function Stop-Epic {
    foreach ($n in 'EpicGamesLauncher.exe', 'EpicWebHelper.exe', 'EpicOnlineServicesUserHelper.exe', 'EpicOnlineServicesHost.exe', 'UnrealCEFSubProcess.exe', 'CrashReportClient.exe') {
        & taskkill.exe /F /T /IM $n 2>$null | Out-Null
    }
    Start-Sleep -Seconds 3
}

function Get-Http([string]$url) {
    try { return (Invoke-WebRequest -UseBasicParsing -TimeoutSec 4 -Uri $url).Content } catch { return $null }
}

function Invoke-CdpBatch {
    param([string]$WsUrl, [array]$Commands, [int]$TimeoutSec = 60, [int]$Trunc = 1400)
    $out = @()
    $ws = [System.Net.WebSockets.ClientWebSocket]::new()
    $cts = [System.Threading.CancellationTokenSource]::new([TimeSpan]::FromSeconds($TimeoutSec))
    try {
        $ws.ConnectAsync([Uri]$WsUrl, $cts.Token).GetAwaiter().GetResult()
        $id = 0
        foreach ($c in $Commands) {
            if ($c.sleepMs) { Start-Sleep -Milliseconds $c.sleepMs; continue }
            $id++
            $payload = @{ id = $id; method = $c.method }
            if ($c.params) { $payload.params = $c.params }
            $json = $payload | ConvertTo-Json -Depth 20 -Compress
            $bytes = [Text.Encoding]::UTF8.GetBytes($json)
            $ws.SendAsync([ArraySegment[byte]]::new($bytes), [System.Net.WebSockets.WebSocketMessageType]::Text, $true, $cts.Token).GetAwaiter().GetResult()
            while ($true) {
                $ms = [IO.MemoryStream]::new()
                do {
                    $buf = [byte[]]::new(65536)
                    $res = $ws.ReceiveAsync([ArraySegment[byte]]::new($buf), $cts.Token).GetAwaiter().GetResult()
                    $ms.Write($buf, 0, $res.Count)
                } while (-not $res.EndOfMessage)
                $text = [Text.Encoding]::UTF8.GetString($ms.ToArray())
                $m = [regex]::Match($text, '"id":(\d+)')
                if ($m.Success -and [int]$m.Groups[1].Value -eq $id) {
                    $short = if ($text.Length -gt $Trunc) { $text.Substring(0, $Trunc) + "...(len=$($text.Length))" } else { $text }
                    $out += [pscustomobject]@{ method = $c.method; response = $short }
                    break
                }
            }
        }
    }
    catch { $out += [pscustomobject]@{ method = 'EXCEPTION'; response = "$($_.Exception.Message)" } }
    finally { $ws.Dispose() }
    foreach ($o in $out) { Write-Host ("  <{0}> {1}" -f $o.method, $o.response) }
}

$EvalInfo = 'JSON.stringify({href:location.href,title:document.title,ua:navigator.userAgent,vis:document.visibilityState,focus:document.hasFocus(),w:innerWidth,h:innerHeight,keys:Object.keys(window).filter(function(k){return /^(ue|epic|cef|launcher|egs)/i.test(k)}).slice(0,40),text:(document.body?document.body.innerText:"").slice(0,200)})'

function Test-Cdp([int]$p, [string]$tag, [bool]$navigate) {
    Section "CDP experiments ($tag) on port $p"
    $ver = Get-Http "http://127.0.0.1:$p/json/version"
    Write-Host "[/json/version] $ver"
    $listRaw = Get-Http "http://127.0.0.1:$p/json/list"
    if (-not $listRaw) { Write-Host "no /json/list"; return }
    $list = @($listRaw | ConvertFrom-Json)
    Write-Host "[/json/list] targets=$($list.Count)"
    foreach ($t in $list) { Write-Host ("  type={0} id={1} title={2} url={3}" -f $t.type, $t.id, $t.title, $t.url) }

    $browserWs = ($ver | ConvertFrom-Json).webSocketDebuggerUrl
    if ($browserWs) {
        Write-Host "[browser endpoint] $browserWs"
        Invoke-CdpBatch -WsUrl $browserWs -Commands @(
            @{ method = 'Browser.getVersion' },
            @{ method = 'Target.getTargets' },
            @{ method = 'Target.createTarget'; params = @{ url = 'about:blank' } }
        )
    }
    Write-Host "[/json/new] $(Get-Http "http://127.0.0.1:$p/json/new?about:blank")"

    $pages = @($list | Where-Object { $_.type -eq 'page' -and $_.webSocketDebuggerUrl })
    foreach ($t in ($pages | Select-Object -First 6)) {
        Write-Host "[page $($t.id)] $($t.url)"
        Invoke-CdpBatch -WsUrl $t.webSocketDebuggerUrl -Commands @(
            @{ method = 'Runtime.evaluate'; params = @{ expression = $EvalInfo; returnByValue = $true } },
            @{ method = 'Page.captureScreenshot'; params = @{ format = 'jpeg'; quality = 30 } }
        ) -Trunc 900
    }

    if ($navigate -and $pages.Count -gt 0) {
        $t = @($pages | Where-Object { $_.url -match 'store' })[0]
        if (-not $t) { $t = $pages[0] }
        Write-Host "[navigate test on $($t.id)] was $($t.url)"
        $storeEval = 'JSON.stringify({href:location.href,title:document.title,nav:(function(){var n=document.querySelector("egs-navigation");return n?{isloggedin:n.getAttribute("isloggedin"),displayname:n.getAttribute("displayname")}:null})(),len:document.documentElement.outerHTML.length,blocked:/just a moment|verify you are human|access denied|captcha/i.test((document.body?document.body.innerText:"").slice(0,3000)),text:(document.body?document.body.innerText:"").replace(/\s+/g," ").slice(0,400)})'
        Invoke-CdpBatch -WsUrl $t.webSocketDebuggerUrl -TimeoutSec 120 -Commands @(
            @{ method = 'Page.enable' },
            @{ method = 'Page.navigate'; params = @{ url = 'https://store.epicgames.com/en-US/free-games' } },
            @{ sleepMs = 20000 },
            @{ method = 'Runtime.evaluate'; params = @{ expression = $storeEval; returnByValue = $true } },
            @{ method = 'Input.dispatchMouseEvent'; params = @{ type = 'mouseMoved'; x = 50; y = 50 } },
            @{ method = 'Page.navigate'; params = @{ url = 'https://store.epicgames.com/purchase?offers=1-invalid-invalid' } },
            @{ sleepMs = 12000 },
            @{ method = 'Runtime.evaluate'; params = @{ expression = $storeEval; returnByValue = $true } }
        )
        $after = Get-Http "http://127.0.0.1:$p/json/list"
        if ($after) { foreach ($x in @($after | ConvertFrom-Json)) { Write-Host ("  after: type={0} title={1} url={2}" -f $x.type, $x.title, $x.url) } }
    }
}

function Wait-Cdp([int]$p, [string]$exe, [string]$launchArgs, [int]$minutes, [scriptblock]$relaunch) {
    $deadline = (Get-Date).AddMinutes($minutes)
    $lastSig = ''
    $relaunches = 0
    $goneSince = $null
    $noFlagSince = $null
    while ((Get-Date) -lt $deadline) {
        if (Get-Http "http://127.0.0.1:$p/json/version") { Write-Host "CDP endpoint is UP on $p after $([int]((Get-Date) - $script:t0).TotalSeconds)s"; return $true }
        $procs = @(Get-EpicProcs)
        $sig = ($procs | ForEach-Object { "$($_.ProcessId):$($_.Name)" }) -join ','
        if ($sig -ne $lastSig) { Show-EpicProcs 'changed'; $lastSig = $sig }
        $main = @($procs | Where-Object { $_.Name -eq 'EpicGamesLauncher.exe' })
        if ($main.Count -eq 0) {
            if (-not $goneSince) { $goneSince = Get-Date }
            if (((Get-Date) - $goneSince).TotalSeconds -gt 45 -and $relaunches -lt 3) {
                Write-Host "launcher not running for 45s -> relaunch #$($relaunches + 1)"
                & $relaunch; $relaunches++; $goneSince = $null
            }
        }
        else {
            $goneSince = $null
            $hasFlag = @($main | Where-Object { "$($_.CommandLine)" -match 'cefdebug' }).Count -gt 0
            if (-not $hasFlag) {
                if (-not $noFlagSince) { $noFlagSince = Get-Date }
                if (((Get-Date) - $noFlagSince).TotalSeconds -gt 150 -and $relaunches -lt 3) {
                    Write-Host "launcher is running WITHOUT -cefdebug for 150s (lost on self-update restart?) -> kill + relaunch #$($relaunches + 1)"
                    Stop-Epic; & $relaunch; $relaunches++; $noFlagSince = $null
                }
            }
            else { $noFlagSince = $null }
        }
        Start-Sleep -Seconds 10
    }
    Write-Host "CDP endpoint did NOT come up on $p within $minutes minutes"
    return $false
}

# ---------------------------------------------------------------- environment
Section 'environment'
Write-Host "os=$([Environment]::OSVersion.VersionString) interactive=$([Environment]::UserInteractive) session=$((Get-Process -Id $PID).SessionId) user=$env:USERNAME ps=$($PSVersionTable.PSVersion)"
try { Add-Type -AssemblyName System.Windows.Forms; Write-Host "screen=$([System.Windows.Forms.Screen]::PrimaryScreen.Bounds)" } catch { Write-Host "screen: $_" }

# ---------------------------------------------------------------- install
Section 'install'
$msi = Join-Path $OutDir 'EpicInstaller.msi'
$sw = [Diagnostics.Stopwatch]::StartNew()
Invoke-WebRequest -UseBasicParsing -Uri 'https://launcher-public-service-prod06.ol.epicgames.com/launcher/api/installer/download/EpicGamesLauncherInstaller.msi' -OutFile $msi
Write-Host "downloaded $([int]((Get-Item $msi).Length / 1MB)) MB in $([int]$sw.Elapsed.TotalSeconds)s"
# Not -Wait: that also waits for every descendant, and the installer may start the launcher.
$p = Start-Process msiexec.exe -ArgumentList "/i `"$msi`" /qn /norestart /l*v `"$OutDir\msi.log`"" -PassThru
if (-not $p.WaitForExit(600000)) { Write-Host "msiexec still running after 10 minutes"; try { $p.Kill() } catch {} }
Write-Host "msiexec exit=$($p.ExitCode) after $([int]$sw.Elapsed.TotalSeconds)s"
Start-Sleep -Seconds 8
Show-EpicProcs 'after-install'

# ---------------------------------------------------------------- detection
Section 'detection: registry'
foreach ($k in @(
        'HKLM:\SOFTWARE\WOW6432Node\Epic Games\EpicGamesLauncher',
        'HKLM:\SOFTWARE\Epic Games\EpicGamesLauncher',
        'HKLM:\SOFTWARE\WOW6432Node\EpicGames\Unreal Engine',
        'HKLM:\SOFTWARE\EpicGames\Unreal Engine',
        'HKCU:\Software\Epic Games\EpicGamesLauncher',
        'HKCU:\Software\Epic Games\EOS',
        'HKLM:\SOFTWARE\Classes\com.epicgames.launcher',
        'HKLM:\SOFTWARE\Classes\com.epicgames.launcher\shell\open\command',
        'HKCU:\Software\Classes\com.epicgames.launcher\shell\open\command',
        'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run',
        'HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Run',
        'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\EpicGamesLauncher.exe'
    )) {
    if (Test-Path $k) {
        Write-Host "[$k]"
        $item = Get-ItemProperty -Path $k
        foreach ($prop in $item.PSObject.Properties) { if ($prop.Name -notmatch '^PS') { Write-Host "    $($prop.Name) = $($prop.Value)" } }
    }
    else { Write-Host "[$k] (missing)" }
}
foreach ($root in 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall', 'HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall') {
    Get-ChildItem $root -ErrorAction SilentlyContinue | ForEach-Object {
        $v = Get-ItemProperty $_.PSPath -ErrorAction SilentlyContinue
        if ("$($v.DisplayName)" -match 'Epic') {
            Write-Host "[uninstall] key=$($_.PSChildName) DisplayName=$($v.DisplayName) Version=$($v.DisplayVersion) InstallLocation=$($v.InstallLocation) Publisher=$($v.Publisher)"
        }
    }
}

Section 'detection: files'
foreach ($d in "${env:ProgramFiles(x86)}\Epic Games", "$env:ProgramFiles\Epic Games", "$env:ProgramData\Epic", "$env:LOCALAPPDATA\EpicGamesLauncher", "$env:LOCALAPPDATA\Epic Games") {
    if (Test-Path $d) {
        Write-Host "[$d]"
        Get-ChildItem $d -Recurse -Depth 3 -ErrorAction SilentlyContinue |
            Where-Object { $_.PSIsContainer -or $_.Name -match '\.(exe|dat|ini|item|json)$' } |
            Select-Object -First 70 | ForEach-Object { Write-Host "    $($_.FullName.Substring($d.Length))" }
    }
    else { Write-Host "[$d] (missing)" }
}
$dat = "$env:ProgramData\Epic\UnrealEngineLauncher\LauncherInstalled.dat"
if (Test-Path $dat) { Write-Host "[LauncherInstalled.dat]"; Get-Content $dat | Select-Object -First 30 | ForEach-Object { Write-Host "    $_" } }

$exe = @(
    "${env:ProgramFiles(x86)}\Epic Games\Launcher\Portal\Binaries\Win64\EpicGamesLauncher.exe",
    "${env:ProgramFiles(x86)}\Epic Games\Launcher\Portal\Binaries\Win32\EpicGamesLauncher.exe",
    "$env:ProgramFiles\Epic Games\Launcher\Portal\Binaries\Win64\EpicGamesLauncher.exe"
) | Where-Object { Test-Path $_ } | Select-Object -First 1
if (-not $exe) { Write-Host "FATAL: EpicGamesLauncher.exe not found"; exit 1 }
$cwd = Split-Path $exe
Write-Host "exe=$exe version=$((Get-Item $exe).VersionInfo.FileVersion) product=$((Get-Item $exe).VersionInfo.ProductVersion)"
Get-ChildItem $cwd -Filter '*.exe' | ForEach-Object { Write-Host "    $($_.Name) $($_.VersionInfo.FileVersion)" }

# ---------------------------------------------------------------- tests
$log = "$env:LOCALAPPDATA\EpicGamesLauncher\Saved\Logs\EpicGamesLauncher.log"
function Show-LauncherLog([string]$tag) {
    Section "launcher log ($tag)"
    if (Test-Path $log) {
        Copy-Item $log (Join-Path $OutDir "EpicGamesLauncher-$tag.log") -ErrorAction SilentlyContinue
        Select-String -Path $log -Pattern 'cefdebug|remote.debug|DevTools|Command ?line|SelfUpdate|Self update|Restarting|LogInit: (Version|Build|Engine)|WebBrowser|RememberMe|silent' |
            Select-Object -First 45 | ForEach-Object { $l = $_.Line; if ($l.Length -gt 260) { $l = $l.Substring(0, 260) }; Write-Host "    $l" }
    }
    else { Write-Host "no launcher log at $log" }
    Get-ChildItem "$env:LOCALAPPDATA\EpicGamesLauncher\Saved" -Recurse -Depth 3 -ErrorAction SilentlyContinue |
        Where-Object { -not $_.PSIsContainer } | Select-Object -First 40 | ForEach-Object { Write-Host "    saved: $($_.FullName.Substring($env:LOCALAPPDATA.Length)) ($($_.Length))" }
}

Stop-Epic
$script:t0 = Get-Date
$up = $false
$closed = 'n/a'

if ($Test -eq 'silent') {
    Section "test: normal desktop, -silent -cefdebug=$Port"
    $launch = { Start-Process -FilePath $exe -ArgumentList '-silent', "-cefdebug=$Port" -WorkingDirectory $cwd }
    & $launch
    $up = Wait-Cdp $Port $exe "-silent -cefdebug=$Port" $BootMinutes $launch
    Show-EpicProcs 'silent'
    Show-Windows 'silent: default desktop (want launcher-VISIBLE=0)' ([IntPtr]::Zero)
    Write-Host "[listening ports of launcher processes]"
    $pidsA = @(Get-EpicProcs | ForEach-Object { $_.ProcessId })
    Get-NetTCPConnection -State Listen -ErrorAction SilentlyContinue | Where-Object { $pidsA -contains $_.OwningProcess } |
        ForEach-Object { Write-Host "    $($_.LocalAddress):$($_.LocalPort) pid=$($_.OwningProcess)" }
    if ($up) {
        Start-Sleep -Seconds 20
        Test-Cdp $Port 'silent on normal desktop' $true
        Show-Windows 'silent: after navigation' ([IntPtr]::Zero)
    }

    Section 'second instance while one is running (what a user double-click does)'
    Start-Process -FilePath $exe -WorkingDirectory $cwd
    for ($i = 0; $i -lt 6; $i++) { Start-Sleep -Seconds 2; Show-EpicProcs "second-instance t+$(($i + 1) * 2)s" }
    Show-Windows 'after second instance (does the window pop up?)' ([IntPtr]::Zero)
    Write-Host "cdp still up: $([bool](Get-Http "http://127.0.0.1:$Port/json/version"))"
}

if ($Test -eq 'desktop') {
    Section "test: hidden desktop, -cefdebug=$Port (no -silent)"
    $deskName = 'EglProbeDesk'
    $hDesk = [WinProbe]::CreateDesktopW($deskName, [IntPtr]::Zero, [IntPtr]::Zero, 0, 0x10000000, [IntPtr]::Zero)
    Write-Host "CreateDesktopW -> $hDesk (lastError=$([Runtime.InteropServices.Marshal]::GetLastWin32Error()))"
    $launch = { $newPid = [WinProbe]::StartOnDesktop($deskName, "`"$exe`" -cefdebug=$Port", $cwd); Write-Host "started pid=$newPid on desktop $deskName" }
    try { & $launch } catch { Write-Host "launch on hidden desktop FAILED: $_" }
    $up = Wait-Cdp $Port $exe "-cefdebug=$Port" $BootMinutes $launch
    Show-EpicProcs 'desktop'
    Show-Windows 'hidden-desktop test: DEFAULT desktop (want launcher-owned=0)' ([IntPtr]::Zero)
    Show-Windows 'hidden-desktop test: HIDDEN desktop' $hDesk
    if ($up) {
        Start-Sleep -Seconds 20
        Test-Cdp $Port 'hidden desktop' $true
        Show-Windows 'hidden-desktop test: DEFAULT desktop after navigation' ([IntPtr]::Zero)
        Show-Windows 'hidden-desktop test: HIDDEN desktop after navigation' $hDesk
    }

    Section 'second instance from the NORMAL desktop while the hidden one runs'
    Start-Process -FilePath $exe -WorkingDirectory $cwd
    for ($i = 0; $i -lt 6; $i++) { Start-Sleep -Seconds 2; Show-EpicProcs "second-instance t+$(($i + 1) * 2)s" }
    Show-Windows 'DEFAULT desktop after second instance' ([IntPtr]::Zero)
    Show-Windows 'HIDDEN desktop after second instance' $hDesk
    [void][WinProbe]::CloseDesktop($hDesk)
}

if ($Test -eq 'plain') {
    Section "test: normal desktop, NO -silent, -cefdebug=$Port (what would the user see?)"
    $launch = { Start-Process -FilePath $exe -ArgumentList "-cefdebug=$Port" -WorkingDirectory $cwd }
    & $launch
    $up = Wait-Cdp $Port $exe "-cefdebug=$Port" $BootMinutes $launch
    Start-Sleep -Seconds 15
    Show-EpicProcs 'plain'
    Show-Windows 'plain: default desktop (expect VISIBLE windows)' ([IntPtr]::Zero)
    if ($up) { Test-Cdp $Port 'plain' $false }

    Section 'graceful close test (taskkill without /F = WM_CLOSE)'
    $swc = [Diagnostics.Stopwatch]::StartNew()
    & taskkill.exe /IM EpicGamesLauncher.exe 2>&1 | ForEach-Object { Write-Host "    $_" }
    $closed = $false
    for ($i = 0; $i -lt 30; $i++) {
        Start-Sleep -Seconds 1
        if (@(Get-EpicProcs | Where-Object { $_.Name -eq 'EpicGamesLauncher.exe' }).Count -eq 0) { $closed = $true; break }
    }
    Write-Host "graceful close worked=$closed after $([int]$swc.Elapsed.TotalSeconds)s"
    Show-EpicProcs 'after-graceful-close'
    Show-Windows 'after graceful close' ([IntPtr]::Zero)
}

Show-LauncherLog $Test
Stop-Epic
Section 'summary'
Write-Host "RESULT test=$Test cdp_up=$up graceful_close=$closed"
exit 0
