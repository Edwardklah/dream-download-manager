# devloop.ps1 - lets Claude run a FIXED set of project commands on this PC
# and read the results, so you don't have to copy output or take screenshots.
#
# Start it once, in its own PowerShell window, from the project folder:
#     .\devloop.ps1
# Leave that window open while we work. Stop it any time with Ctrl+C.
#
# How it works: Claude writes a small request file into the .dev folder.
# This script picks it up, runs the matching action, and writes the output to
# .dev\result-<id>.txt for Claude to read. Every run is also shown in this window.
#
# Only the actions in the switch statement below can run. A request for
# anything else is refused. To see or change what's allowed, read that section.

$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$dev = Join-Path $root '.dev'
$downloads = Join-Path $dev 'downloads'   # test downloads go here, not your Downloads folder
New-Item -ItemType Directory -Force -Path $dev, $downloads | Out-Null
Set-Location $root

$utf8 = New-Object System.Text.UTF8Encoding($false)

# The browser extension's helper, built next to the app.
$hostExe = Join-Path $root 'gui\bin\dlm-host.exe'

# The local test server (bench-* actions).
$benchDir = Join-Path $dev 'bench'
$benchExe = Join-Path $benchDir 'bench.exe'
$benchLog = Join-Path $benchDir 'log.jsonl'

# Stops the bench server, if it's running - only our own bench.exe.
function Stop-Bench {
    Get-Process -Name 'bench' -ErrorAction SilentlyContinue |
        Where-Object { $_.Path -eq $benchExe } |
        Stop-Process -Force
    Start-Sleep -Milliseconds 300
}

function Write-Result([string]$id, [string]$text, [int]$exitCode) {
    $file = Join-Path $dev "result-$id.txt"
    [System.IO.File]::WriteAllText($file, $text, $utf8)
    # The .done marker is written LAST, so Claude never reads a half-written result.
    [System.IO.File]::WriteAllText((Join-Path $dev "result-$id.done"), "$exitCode", $utf8)
}

# Runs "go <arguments>" in the project folder.
function Invoke-Go([string[]]$goArgs) {
    return Invoke-Cmd "go $($goArgs -join ' ')" $root
}

# Runs a FIXED command line (built by Invoke-Request, never taken from a
# request as-is) in a folder, and returns @(exitCode, combinedOutput).
# Output goes straight to .dev\run.stdout while it runs, so Claude can watch a
# long job's progress. Claude can stop a running job by creating .dev\cancel.
function Invoke-Cmd([string]$commandLine, [string]$workDir) {
    $out = Join-Path $dev 'run.stdout'
    $err = Join-Path $dev 'run.stderr'
    $cancelFile = Join-Path $dev 'cancel'
    Remove-Item $cancelFile -ErrorAction SilentlyContinue   # ignore stale cancels

    # The shell (cmd on Windows) runs the command and redirects its output to
    # the files. Arguments were validated by Invoke-Request, and URLs and paths
    # are quoted. (The outer parentheses make the redirect cover every command
    # in a "a & b & c" line.)
    $command = "($commandLine) > `"$out`" 2> `"$err`""
    $onWindows = ($env:OS -eq 'Windows_NT')
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    if ($onWindows) {
        $psi.FileName = 'cmd.exe'
        $psi.Arguments = "/S /C `"$command`""
    } else {
        $psi.FileName = 'sh'
        $psi.ArgumentList.Add('-c')
        $psi.ArgumentList.Add($command)
    }
    $psi.WorkingDirectory = $workDir
    $psi.UseShellExecute = $false
    $p = [System.Diagnostics.Process]::Start($psi)

    # Wait for it to finish, but stop it early if Claude drops a "cancel" file.
    $cancelled = $false
    while (-not $p.HasExited) {
        if (Test-Path $cancelFile) {
            Remove-Item $cancelFile -ErrorAction SilentlyContinue
            Write-Host "Cancel requested - stopping." -ForegroundColor Yellow
            if ($onWindows) {
                # /T stops cmd, go, and the program go started.
                & taskkill /PID $p.Id /T /F | Out-Null
            } else {
                & pkill -TERM -P $p.Id   # the shell's children first
                Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
            }
            $cancelled = $true
            break
        }
        Start-Sleep -Milliseconds 300
    }
    $p.WaitForExit()
    $code = $p.ExitCode
    Start-Sleep -Milliseconds 300   # let the files be released

    $text = ''
    if ($cancelled) { $text += "(cancelled by request)`n"; $code = -2 }
    if (Test-Path $out) { $text += [System.IO.File]::ReadAllText($out) }
    if (Test-Path $err) {
        $e = [System.IO.File]::ReadAllText($err)
        if ($e.Trim()) { $text += "`n--- stderr ---`n" + $e }
    }
    return @($code, $text)
}

$gui = Join-Path $root 'gui'

# The built app: the .exe in gui\bin.
function Get-AppExe {
    $bin = Join-Path $gui 'bin'
    if (-not (Test-Path $bin)) { return $null }
    # DreamDM.exe (gui.exe before the app had a name) - not the helper, dlm-host.exe.
    foreach ($name in 'DreamDM.exe', 'gui.exe') {
        $p = Join-Path $bin $name
        if (Test-Path $p) { return Get-Item $p }
    }
    return $null
}

# Stops the built app if it's running (and the WebView2 processes it owns).
function Stop-App {
    $exe = Get-AppExe
    if (-not $exe) { return }
    $name = [System.IO.Path]::GetFileNameWithoutExtension($exe.Name)
    Get-Process -Name $name -ErrorAction SilentlyContinue | ForEach-Object {
        & taskkill /PID $_.Id /T /F | Out-Null
    }
}

# Reports the app's file size and memory, including its WebView2 child
# processes (which is where the window's browser engine lives).
function Measure-App {
    $exe = Get-AppExe
    if (-not $exe) { return "no built app in gui\bin`n" }
    $text = "exe: {0}  {1:N1} MB`n" -f $exe.Name, ($exe.Length / 1MB)
    $name = [System.IO.Path]::GetFileNameWithoutExtension($exe.Name)
    $main = Get-Process -Name $name -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $main) { return $text + "app is not running`n" }

    # Every process whose parent chain leads back to the app.
    $all = Get-CimInstance Win32_Process | Select-Object ProcessId, ParentProcessId, Name
    $family = @($main.Id)
    $added = $true
    while ($added) {
        $added = $false
        foreach ($p in $all) {
            if (($family -contains $p.ParentProcessId) -and -not ($family -contains $p.ProcessId)) {
                $family += $p.ProcessId; $added = $true
            }
        }
    }
    # "Task Manager MB" is the private working set: RAM in use that belongs to
    # this process alone - the number Task Manager's Memory column shows.
    # "working MB" also counts shared system libraries; "committed MB" is
    # memory Windows has promised the process, most of it never used.
    # Read it straight from Windows (GetProcessMemoryInfo), which also works
    # for WebView2's sandboxed processes, unlike the performance counters.
    if (-not ('DevLoopMem' -as [type])) {
        Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
public static class DevLoopMem {
    [StructLayout(LayoutKind.Sequential)]
    struct PMC2 {
        public uint cb; public uint PageFaultCount;
        public UIntPtr PeakWorkingSetSize, WorkingSetSize, QuotaPeakPagedPoolUsage, QuotaPagedPoolUsage,
            QuotaPeakNonPagedPoolUsage, QuotaNonPagedPoolUsage, PagefileUsage, PeakPagefileUsage,
            PrivateUsage, PrivateWorkingSetSize;
        public ulong SharedCommitUsage;
    }
    [DllImport("kernel32.dll")] static extern IntPtr OpenProcess(uint access, bool inherit, int pid);
    [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr h);
    [DllImport("psapi.dll")] static extern bool GetProcessMemoryInfo(IntPtr h, ref PMC2 c, uint size);
    public static double PrivateWorkingSet(int pid) {
        IntPtr h = OpenProcess(0x1000, false, pid); // PROCESS_QUERY_LIMITED_INFORMATION
        if (h == IntPtr.Zero) return -1;
        try {
            PMC2 c = new PMC2(); c.cb = (uint)Marshal.SizeOf(typeof(PMC2));
            if (!GetProcessMemoryInfo(h, ref c, c.cb)) return -1;
            return (double)c.PrivateWorkingSetSize.ToUInt64();
        } finally { CloseHandle(h); }
    }
}
"@
    }
    $perf = @{}
    foreach ($id in $family) { $perf[[int]$id] = [DevLoopMem]::PrivateWorkingSet([int]$id) }
    $totalPWS = 0; $totalWS = 0; $totalPriv = 0
    $text += "`n{0,-18} {1,7} {2,16} {3,12} {4,13}`n" -f 'process', 'pid', 'Task Manager MB', 'working MB', 'committed MB'
    foreach ($id in $family) {
        $p = Get-Process -Id $id -ErrorAction SilentlyContinue
        if (-not $p) { continue }
        $pws = 0; if ($perf.ContainsKey($id)) { $pws = $perf[$id] }
        $totalPWS += $pws; $totalWS += $p.WorkingSet64; $totalPriv += $p.PrivateMemorySize64
        $text += "{0,-18} {1,7} {2,16:N1} {3,12:N1} {4,13:N1}`n" -f $p.ProcessName, $p.Id, ($pws / 1MB), ($p.WorkingSet64 / 1MB), ($p.PrivateMemorySize64 / 1MB)
    }
    $text += "{0,-18} {1,7} {2,16:N1} {3,12:N1} {4,13:N1}`n" -f 'TOTAL', '', ($totalPWS / 1MB), ($totalWS / 1MB), ($totalPriv / 1MB)
    return $text
}

function Invoke-Request($req) {
    switch ($req.action) {
        'test' {
            # Run the tests. Optional "run": only tests whose name matches.
            $a = @('test', './...', '-count=1', '-v')
            if ($req.run) {
                if ($req.run -notmatch '^[A-Za-z0-9_|^$.*]+$') { throw "invalid test name pattern" }
                $a += @('-run', "`"$($req.run)`"")
            }
            return Invoke-Go $a
        }
        'vet' { return Invoke-Go @('vet', './...') }
        'build' { return Invoke-Go @('build', '-o', 'dl.exe', './cmd/dl') }
        'version' { return Invoke-Go @('version') }
        'dl' {
            # Download a file into .dev\downloads. Optional "stopAfter" like "10s"
            # cancels it partway (as if Ctrl+C was pressed), to test resume.
            # Optional "connections" (1-32) sets how many connections to use.
            $url = [string]$req.url
            if ($url -notmatch '^https?://[^\s"`]+$') { throw "invalid URL" }
            $a = @('run', './cmd/dl')
            if ($req.connections) {
                if ([string]$req.connections -notmatch '^\d{1,2}$') { throw "invalid connections" }
                $a += @('-c', [string]$req.connections)
            }
            if ($req.stopAfter) {
                if ($req.stopAfter -notmatch '^\d+(ms|s|m)$') { throw "invalid stopAfter" }
                $a += @('-stop-after', $req.stopAfter)
            }
            $a += @("`"$url`"", "`"$downloads`"")
            return Invoke-Go $a
        }
        'list-downloads' {
            $files = Get-ChildItem -Path $downloads -File |
                ForEach-Object { '{0,14:N0}  {1}' -f $_.Length, $_.Name }
            return @(0, (($files -join "`n") + "`n"))
        }
        'limits' {
            # Show the servers whose connection limits are remembered.
            return Invoke-Go @('run', './cmd/dl', '-limits')
        }
        'hash-downloads' {
            # SHA-256 fingerprint of each test download: identical files give
            # identical fingerprints.
            $lines = Get-ChildItem -Path $downloads -File | ForEach-Object {
                '{0}  {1,14:N0}  {2}' -f (Get-FileHash $_.FullName -Algorithm SHA256).Hash, $_.Length, $_.Name
            }
            return @(0, (($lines -join "`n") + "`n"))
        }
        'clean-downloads' {
            # Only ever touches the .dev\downloads test folder.
            Get-ChildItem -Path $downloads -File | Remove-Item -Force
            return @(0, "Emptied .dev\downloads`n")
        }

        # ---- The desktop app (the "gui" folder) ----

        'tools' {
            # Versions of the tools the desktop app needs.
            return Invoke-Cmd 'go version & node --version & npm --version & wails3 version' $root
        }
        'app-init' {
            # Create the desktop app project from Wails' plain TypeScript
            # template. Refuses if the gui folder already exists.
            if (Test-Path $gui) { throw "the gui folder already exists" }
            return Invoke-Cmd 'wails3 init -n gui -t vanilla -mod dlmanager/gui' $root
        }
        'app-flatten' {
            # wails3 init created gui\gui; move its contents up into gui.
            $inner = Join-Path $gui 'gui'
            if (-not (Test-Path $inner)) { throw "nothing to flatten" }
            Get-ChildItem -Path $inner -Force | Move-Item -Destination $gui
            Remove-Item $inner   # now empty; fails safely if anything was left
            $list = Get-ChildItem -Path $gui -Recurse -File -Force |
                Where-Object { $_.FullName -notmatch '\\node_modules\\' } |
                ForEach-Object { $_.FullName.Substring($gui.Length + 1) }
            return @(0, (($list -join "`n") + "`n"))
        }
        'app-tidy' {
            # Fetch/clean up the app's Go dependencies.
            return Invoke-Cmd 'go mod tidy' $gui
        }
        'app-build' {
            # Build the app (installs frontend packages with npm, builds the
            # frontend, then the Go program) into gui\bin, plus dlm-host.exe
            # (the browser extension's helper) next to it.
            Stop-App
            $built = Invoke-Cmd 'wails3 build' $gui
            if ($built[0] -ne 0) { return $built }
            $hostBuilt = Invoke-Cmd "go build -ldflags=`"-s -w`" -o `"$hostExe`" ./cmd/dlm-host" $root
            return @($hostBuilt[0], ($built[1] + "`n--- dlm-host ---`n" + $hostBuilt[1]))
        }
        'ext-setup' {
            # Register dlm-host with Chrome and Edge, so the extension can
            # reach the app. Only writes this user's settings (HKCU).
            if (-not (Test-Path $hostExe)) { throw "run app-build first" }
            return Invoke-Cmd "`"$hostExe`" -install" $root
        }
        'ext-remove' {
            if (-not (Test-Path $hostExe)) { throw "dlm-host.exe not found" }
            return Invoke-Cmd "`"$hostExe`" -uninstall" $root
        }
        'app-start' {
            # Start the built app WITHOUT waiting for it to close.
            Stop-App
            $exe = Get-AppExe
            if (-not $exe) { throw "no built app in gui\bin - run app-build first" }
            # Diagnostics on: the app writes Go's memory statistics to .dev\diag.txt.
            $env:DLM_DIAG = Join-Path $dev 'diag.txt'
            $startArgs = @()
            if ($req.tray) { $startArgs = @('--tray') }   # start with no window
            if ($startArgs.Count -gt 0) {
                $p = Start-Process -FilePath $exe.FullName -ArgumentList $startArgs -WorkingDirectory $exe.DirectoryName -PassThru
            } else {
                $p = Start-Process -FilePath $exe.FullName -WorkingDirectory $exe.DirectoryName -PassThru
            }
            Remove-Item Env:\DLM_DIAG
            Start-Sleep -Seconds 3
            return @(0, "started $($exe.Name) $($startArgs -join ' ') (pid $($p.Id))`n")
        }
        'app-stop' {
            Stop-App
            return @(0, "stopped`n")
        }
        'app-installer' {
            # Build the setup file, dist\DreamDM-Setup-<version>.exe, from the
            # app already built by app-build. Needs NSIS (winget install NSIS.NSIS).
            $makensis = Join-Path ${env:ProgramFiles(x86)} 'NSIS\makensis.exe'
            if (-not (Test-Path $makensis)) { $makensis = Join-Path $env:ProgramFiles 'NSIS\makensis.exe' }
            if (-not (Test-Path $makensis)) { throw "NSIS not found - run: winget install -e --id NSIS.NSIS" }
            if (-not (Test-Path (Join-Path $gui 'bin\DreamDM.exe'))) { throw "run app-build first" }
            $info = Get-Content (Join-Path $gui 'build\windows\info.json') -Raw | ConvertFrom-Json
            $version = $info.fixed.file_version
            if ($version -notmatch '^\d+\.\d+\.\d+$') { throw "odd version in info.json: $version" }
            New-Item -ItemType Directory -Force -Path (Join-Path $root 'dist') | Out-Null
            $built = Invoke-Cmd "`"$makensis`" /V2 /DVERSION=$version DreamDM.nsi" (Join-Path $root 'installer\windows')
            $setup = Join-Path $root "dist\DreamDM-Setup-$version.exe"
            if ($built[0] -eq 0 -and (Test-Path $setup)) {
                $built[1] += "`n$setup  {0:N1} MB`n" -f ((Get-Item $setup).Length / 1MB)
            }
            return $built
        }
        'setup-start' {
            # Start the newest setup file built by app-installer (without
            # waiting; it's clicked through on screen).
            $setup = Get-ChildItem -Path (Join-Path $root 'dist') -Filter 'DreamDM-Setup-*.exe' -File -ErrorAction SilentlyContinue |
                Sort-Object LastWriteTime -Descending | Select-Object -First 1
            if (-not $setup) { throw "no setup file - run app-installer first" }
            Start-Process -FilePath $setup.FullName | Out-Null
            return @(0, "started $($setup.Name)`n")
        }
        'probe-video' {
            # Inspect a downloaded video with ffprobe/ffmpeg (read-only): its
            # streams, the first 20 seconds of picture packets, and any
            # decoding errors in the first minute. The file must be inside
            # your Downloads folder; give its path relative to it.
            $rel = [string]$req.file
            if (-not $rel -or $rel -match '\.\.' -or [System.IO.Path]::IsPathRooted($rel)) { throw "give a path inside Downloads, like Video\name.mp4" }
            $downloadsDir = Join-Path $env:USERPROFILE 'Downloads'
            $file = [System.IO.Path]::GetFullPath((Join-Path $downloadsDir $rel))
            if (-not $file.StartsWith($downloadsDir + '\') -or -not (Test-Path -LiteralPath $file -PathType Leaf)) { throw "not found in Downloads: $rel" }
            $q = '"' + $file + '"'
            $a = Invoke-Cmd "ffprobe -v error -show_entries format=format_name,duration,start_time:stream=index,codec_type,codec_name,profile,level,width,height,pix_fmt,has_b_frames,r_frame_rate,avg_frame_rate,time_base,start_time,duration,nb_frames -of compact $q" $root
            $b = Invoke-Cmd "ffprobe -v error -select_streams v:0 -read_intervals %+20 -show_entries packet=pts_time,dts_time,duration_time,size,flags -of compact $q" $root
            $c = Invoke-Cmd "ffmpeg -hide_banner -v error -t 60 -i $q -f null - 2>&1" $root
            $errLines = ($c[1] -split "`n" | Select-Object -First 60) -join "`n"
            return @(0, "--- streams ---`n$($a[1])`n--- picture packets, first 20 s ---`n$($b[1])`n--- decoding errors, first minute ---`n$errLines`n")
        }
        'convert-test' {
            # Check one downloaded piece of an HLS video (inside Downloads):
            # its first bytes, whether ffmpeg decodes it cleanly, and whether
            # it still does after our converter (cmd/tsmp4) has made it an
            # MP4. Writes only .dev\convert-test.mp4.
            # With no file given: piece number $req.piece (default 1) of the
            # newest pieces folder (a paused HLS download) in Downloads.
            $downloadsDir = Join-Path $env:USERPROFILE 'Downloads'
            $rel = [string]$req.file
            if (-not $rel) {
                $dir = Get-ChildItem -Path $downloadsDir -Directory -Recurse -Depth 2 -Filter '*.pieces' -ErrorAction SilentlyContinue |
                    Sort-Object LastWriteTime -Descending | Select-Object -First 1
                if (-not $dir) { throw "no pieces folder in Downloads - start an HLS video and pause it" }
                $n = 1; if ($req.piece -ne $null) { $n = [int]$req.piece }
                $rel = $dir.FullName.Substring($downloadsDir.Length + 1) + '\' + ('{0:d6}' -f $n)
            }
            if ($rel -match '\.\.' -or [System.IO.Path]::IsPathRooted($rel)) { throw "give a path inside Downloads" }
            $file = [System.IO.Path]::GetFullPath((Join-Path $downloadsDir $rel))
            if (-not $file.StartsWith($downloadsDir + '\') -or -not (Test-Path -LiteralPath $file -PathType Leaf)) { throw "not found in Downloads: $rel" }
            $bytes = [System.IO.File]::ReadAllBytes($file)
            $head = ($bytes[0..([Math]::Min(63, $bytes.Length - 1))] | ForEach-Object { $_.ToString('x2') }) -join ' '
            $syncAt = [Array]::IndexOf($bytes, [byte]0x47)
            $q = '"' + $file + '"'
            $out = Join-Path $dev 'convert-test.mp4'
            Remove-Item $out -ErrorAction SilentlyContinue
            $probe = Invoke-Cmd "ffprobe -v error -show_entries format=format_name,duration:stream=index,codec_name,profile,width,height -of compact $q" $root
            $src = Invoke-Cmd "ffmpeg -hide_banner -v error -i $q -f null - 2>&1" $root
            $conv = Invoke-Cmd "go run ./cmd/tsmp4 $q `"$out`"" $root
            $dst = @(0, '(not converted)')
            if (Test-Path $out) { $dst = Invoke-Cmd "ffmpeg -hide_banner -v error -i `"$out`" -f null - 2>&1" $root }
            $text = "piece: $rel`nsize: $($bytes.Length) bytes; first 0x47 at byte $syncAt`nfirst 64 bytes: $head`n"
            $text += "--- ffprobe ---`n$($probe[1])`n--- the piece as the site sent it: decoding errors ---`n$((($src[1] -split "`n") | Select-Object -First 25) -join "`n")`n"
            $text += "--- our converter ---`n$($conv[1])`n--- after converting: decoding errors ---`n$((($dst[1] -split "`n") | Select-Object -First 25) -join "`n")`n"
            return @(0, $text)
        }
        'app-measure' {
            # Size of the app, and memory of the app plus the WebView2
            # processes it started.
            $text = Measure-App
            $diag = Join-Path $dev 'diag.txt'
            if (Test-Path $diag) { $text += "`nGo's own view:`n" + [System.IO.File]::ReadAllText($diag) }
            return @(0, $text)
        }

        'bench-start' {
            # Build and start the local test server (cmd\bench) in its own
            # window. It only listens on 127.0.0.1, so it uses no internet and
            # nothing outside this PC can reach it. Optional "size" in MB.
            Stop-Bench
            $size = '40'
            if ($req.size) {
                if ("$($req.size)" -notmatch '^\d{1,4}$') { throw "bad size" }
                $size = "$($req.size)"
            }
            New-Item -ItemType Directory -Force -Path $benchDir | Out-Null
            $built = Invoke-Cmd "go build -o `"$benchExe`" ./cmd/bench" $root
            if ($built[0] -ne 0) { return $built }
            $p = Start-Process -FilePath $benchExe -ArgumentList @('-size', $size, '-log', "`"$benchLog`"") -WorkingDirectory $benchDir -PassThru
            Start-Sleep -Seconds 2
            if ($p.HasExited) { throw "the bench server stopped straight away (is port 8090 in use?)" }
            return @(0, "bench running (pid $($p.Id)): open http://127.0.0.1:8090/ for the links`n")
        }
        'bench-stop' {
            Stop-Bench
            return @(0, "stopped`n")
        }
        'bench-report' {
            # Summarise the request log. Optional "last": only the last N runs.
            if (-not (Test-Path $benchExe)) { throw "run bench-start first" }
            if (-not (Test-Path $benchLog)) { return @(0, "no log yet`n") }
            $lastArg = ''
            if ($req.last) {
                if ("$($req.last)" -notmatch '^\d{1,3}$') { throw "bad last" }
                $lastArg = " -last $($req.last)"
            }
            return Invoke-Cmd "`"$benchExe`" -report `"$benchLog`"$lastArg" $root
        }
        'bench-clear-log' {
            # Deletes only the bench's own log file.
            Remove-Item $benchLog -ErrorAction SilentlyContinue
            return @(0, "log cleared`n")
        }

        default { throw "action '$($req.action)' is not allowed" }
    }
}

Write-Host "devloop running in $root" -ForegroundColor Green
Write-Host "Waiting for requests from Claude. Ctrl+C to stop." -ForegroundColor Green

# If this script file is updated, reload it automatically so the new version
# takes effect without you having to restart it.
$scriptStamp = (Get-Item $PSCommandPath).LastWriteTimeUtc

while ($true) {
    if ((Get-Item $PSCommandPath).LastWriteTimeUtc -ne $scriptStamp) {
        Start-Sleep -Milliseconds 500   # let the update finish writing
        Write-Host "devloop.ps1 was updated - reloading." -ForegroundColor Yellow
        & $PSCommandPath
        return
    }
    $requests = Get-ChildItem -Path $dev -Filter 'request-*.json' -File -ErrorAction SilentlyContinue |
        Sort-Object Name
    foreach ($file in $requests) {
        $id = $file.BaseName.Substring('request-'.Length)
        if ($id -notmatch '^[A-Za-z0-9-]+$') { Remove-Item $file.FullName; continue }
        Start-Sleep -Milliseconds 200   # let the file finish being written
        try {
            $req = [System.IO.File]::ReadAllText($file.FullName) | ConvertFrom-Json
        } catch {
            continue   # not fully written yet; try again next pass
        }
        Remove-Item $file.FullName

        $label = "$($req.action) $($req.url) $($req.run) $($req.stopAfter)".Trim()
        if ($req.connections) { $label += " (connections: $($req.connections))" }
        Write-Host ""
        Write-Host "[$(Get-Date -Format HH:mm:ss)] Running: $label" -ForegroundColor Cyan
        $started = Get-Date
        try {
            $r = Invoke-Request $req
            $code = $r[0]; $text = $r[1]
        } catch {
            $code = -1; $text = "devloop refused or failed: $($_.Exception.Message)`n"
        }
        $secs = [math]::Round(((Get-Date) - $started).TotalSeconds, 1)
        $header = "action: $label`nexit: $code`nseconds: $secs`n`n"
        Write-Host $text
        $color = 'Green'
        if ($code -ne 0) { $color = 'Red' }
        Write-Host "[$(Get-Date -Format HH:mm:ss)] Finished: exit $code in ${secs}s" -ForegroundColor $color
        Write-Result $id ($header + $text) $code
    }
    Start-Sleep -Milliseconds 700
}
