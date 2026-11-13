package connections

import "github.com/nickheyer/icebath/pkg/desktop/backend"

// Defines marker_judge and marker_verdict for the host system Python of an SSH remote
var RemoteMarkerJudgePy = "\n" +
	"import os,re,sys,time\n" +
	"MARKER_INT_RE=re.compile(r'[0-9]+')\n" +
	"MARKER_CT_RE=re.compile(r'ct:([0-9]+(?:\\.[0-9]+)?)')\n" +
	"MARKER_DELEGATE_RE=re.compile(r'delegate:([0-9]+) ct:([0-9]+(?:\\.[0-9]+)?)')\n" +
	"\n" +
	"def marker_int(text):\n" +
	"    # int() refuses >4300 digits; past 20 significant digits nothing fits u64 anyway.\n" +
	"    text=text.lstrip('0') or '0'\n" +
	"    return int(text) if len(text)<=20 else None\n" +
	"\n" +
	"def marker_identity_state(pid,ct,started,env):\n" +
	"    if pid==0:return 'dead'\n" +
	"    if pid==env['our_pid']:\n" +
	"        own=env['our_ct']()\n" +
	"        return 'ours' if ct is not None and own is not None and abs(ct-own)<=" +
	jsNumber(backend.OwnCTEpsilonSeconds) +
	" else 'dead'\n" +
	"    if not env['alive'](pid):return 'dead'\n" +
	"    actual=None if ct is None else env['ct'](pid)\n" +
	"    if ct is None or actual is None:return 'unknown' if env['now']-started<=" +
	jsNumber(backend.V1MaxAgeSeconds) +
	" else 'dead'\n" +
	"    return 'match' if abs(ct-actual)<=" +
	jsNumber(backend.CreateTimeToleranceSecs) +
	" else 'dead'\n" +
	"\n" +
	"def marker_judge(text,env):\n" +
	"    if text.startswith('\\ufeff'):text=text[1:]\n" +
	"    lines=[(line[:-1] if line.endswith('\\r') else line).strip(' \\t') for line in text.split('\\n')]\n" +
	"    if len(lines)<2 or not MARKER_INT_RE.fullmatch(lines[0]) or not MARKER_INT_RE.fullmatch(lines[1]):return 'malformed',None\n" +
	"    pid=marker_int(lines[0]);started=marker_int(lines[1])\n" +
	"    if pid is None or pid>4294967295 or started is None or started>18446744073709551615:return 'malformed',None\n" +
	"    ct=MARKER_CT_RE.fullmatch(lines[2]) if len(lines)>2 else None\n" +
	"    ids=[(pid,float(ct.group(1)) if ct else None)]\n" +
	"    for line in lines[3:]:\n" +
	"        delegate=MARKER_DELEGATE_RE.fullmatch(line)\n" +
	"        delegate_pid=marker_int(delegate.group(1)) if delegate else None\n" +
	"        if delegate_pid is not None and delegate_pid<=4294967295:\n" +
	"            ids.append((delegate_pid,float(delegate.group(2))));break\n" +
	"    states=[marker_identity_state(p,c,started,env) for p,c in ids]\n" +
	"    live=[p for (p,_),state in zip(ids,states) if state!='dead']\n" +
	"    return ('ours' if 'ours' in states else 'live' if live else 'dead'),(live[0] if live else None)\n" +
	"\n" +
	"def marker_stat(pid):\n" +
	"    raw=open('/proc/%d/stat'%pid).read()\n" +
	"    return raw[raw.rfind(')')+2:].split()\n" +
	"\n" +
	"def marker_win(pid):\n" +
	"    # (alive, creation unix seconds or None); an open we are denied is alive with no ct.\n" +
	"    import ctypes\n" +
	"    from ctypes import wintypes\n" +
	"    k=ctypes.WinDLL('kernel32',use_last_error=True)\n" +
	"    k.OpenProcess.argtypes=[wintypes.DWORD,wintypes.BOOL,wintypes.DWORD];k.OpenProcess.restype=wintypes.HANDLE\n" +
	"    k.GetExitCodeProcess.argtypes=[wintypes.HANDLE,ctypes.POINTER(wintypes.DWORD)]\n" +
	"    k.GetProcessTimes.argtypes=[wintypes.HANDLE]+[ctypes.POINTER(wintypes.FILETIME)]*4\n" +
	"    k.CloseHandle.argtypes=[wintypes.HANDLE]\n" +
	"    handle=k.OpenProcess(0x1000,False,pid)\n" +
	"    if not handle:return ctypes.get_last_error()!=87,None\n" +
	"    try:\n" +
	"        code=wintypes.DWORD();times=[wintypes.FILETIME() for _ in range(4)]\n" +
	"        if k.GetExitCodeProcess(handle,ctypes.byref(code)) and code.value!=259:return False,None\n" +
	"        if not k.GetProcessTimes(handle,*[ctypes.byref(t) for t in times]):return True,None\n" +
	"        return True,((times[0].dwHighDateTime<<32)|times[0].dwLowDateTime)/1e7-11644473600\n" +
	"    finally:k.CloseHandle(handle)\n" +
	"\n" +
	"def marker_alive(pid):\n" +
	"    if os.name=='nt':return marker_win(pid)[0]\n" +
	"    try:os.kill(pid,0)\n" +
	"    except (ProcessLookupError,OverflowError):return False\n" +
	"    except OSError:pass  # EPERM or unprovable: alive (fail closed)\n" +
	"    try:return marker_stat(pid)[0]!='Z'\n" +
	"    except (OSError,IndexError):return True\n" +
	"\n" +
	"def marker_ct(pid):\n" +
	"    # psutil.create_time() without psutil; unreadable => None => the v1 age ceiling.\n" +
	"    try:\n" +
	"        if os.name=='nt':return marker_win(pid)[1]\n" +
	"        if sys.platform.startswith('linux'):\n" +
	"            with open('/proc/stat') as stat:btime=next(int(line.split()[1]) for line in stat if line.startswith('btime '))\n" +
	"            return btime+int(marker_stat(pid)[19])/os.sysconf('SC_CLK_TCK')\n" +
	"        # UTC like update_lock._stdlib_create_time: a local-time lstart is ambiguous in the repeated DST hour.\n" +
	"        import calendar,subprocess\n" +
	"        out=subprocess.check_output(['ps','-o','lstart=','-p',str(pid)],env=dict(os.environ,LC_ALL='C',TZ='UTC0'),universal_newlines=True)\n" +
	"        return float(calendar.timegm(time.strptime(' '.join(out.split()),'%a %b %d %H:%M:%S %Y')))\n" +
	"    except Exception:return None\n" +
	"\n" +
	"MARKER_ENV={'our_pid':os.getpid(),'our_ct':lambda:marker_ct(os.getpid()),'alive':marker_alive,'ct':marker_ct,'now':time.time()}\n" +
	"\n" +
	"def marker_verdict(raw):\n" +
	"    if raw is None:return 'CLEAR'\n" +
	"    if len(raw)>4096:return 'UNCERTAIN'\n" +
	"    verdict,owner=marker_judge(raw.decode('utf-8','replace'),MARKER_ENV)\n" +
	"    return 'CLEAR' if verdict=='dead' else 'UNCERTAIN' if verdict=='malformed' else 'LIVE:%d'%owner\n"

// POSIX gate holding the updaters lock, printing the verdict or running the payload under it
var RemoteMarkerGatePy = RemoteMarkerJudgePy +
	"\n" +
	"import fcntl,subprocess\n" +
	"marker=sys.argv[1]\n" +
	"payload=sys.argv[2] if len(sys.argv)>2 and sys.argv[2] else None\n" +
	"if payload is None and not os.path.lexists(marker):\n" +
	"    print('CLEAR');sys.exit(0)\n" +
	"\n" +
	"def hold(fd):\n" +
	"    deadline=time.monotonic()+10\n" +
	"    while True:\n" +
	"        try:\n" +
	"            fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB);return True\n" +
	"        except BlockingIOError:\n" +
	"            if time.monotonic()>=deadline:return False\n" +
	"            time.sleep(0.02)\n" +
	"\n" +
	"def read_marker():\n" +
	"    try:\n" +
	"        with open(marker,'rb') as stream:return stream.read(4097)\n" +
	"    except FileNotFoundError:return None\n" +
	"\n" +
	"def checkout_lock(root):\n" +
	"    # hermes_cli/update_lock.py::checkout_lock_path: <git common dir>/hermes-update.lock, else <root>/.icebath-update.lock.\n" +
	"    plain=os.path.join(root,'.icebath-update.lock');dot=os.path.join(root,'.git')\n" +
	"    try:\n" +
	"        if os.path.isdir(dot):gitdir=dot\n" +
	"        elif os.path.isfile(dot):\n" +
	"            with open(dot,encoding='utf-8-sig') as stream:text=stream.read().strip()\n" +
	"            if not text.startswith('gitdir:'):return plain\n" +
	"            gitdir=os.path.join(root,text[len('gitdir:'):].strip())\n" +
	"        else:return plain\n" +
	"        common=os.path.join(gitdir,'commondir')\n" +
	"        if os.path.isfile(common):\n" +
	"            with open(common,encoding='utf-8-sig') as stream:gitdir=os.path.join(gitdir,stream.read().strip())\n" +
	"    except (OSError,ValueError):return plain\n" +
	"    return os.path.join(os.path.normpath(gitdir),'hermes-update.lock')\n" +
	"\n" +
	"def checkout_held():\n" +
	"    # update_lock.checkout_lock_held: take the flock for one try and drop it. A lock file that\n" +
	"    # exists but cannot be opened or probed counts as held: reclaim needs a provably free checkout.\n" +
	"    roots=[os.path.join(os.path.dirname(marker),'icebath-agent')]\n" +
	"    roots+=[os.path.dirname(os.path.dirname(os.path.dirname(os.path.realpath(os.path.expanduser(exe))))) for exe in sys.argv[3:] if exe]\n" +
	"    for root in roots:\n" +
	"        try:lock=os.open(checkout_lock(root),os.O_RDONLY|os.O_CLOEXEC)\n" +
	"        except (FileNotFoundError,NotADirectoryError):continue\n" +
	"        except OSError:return True\n" +
	"        try:fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)\n" +
	"        except OSError:return True\n" +
	"        finally:os.close(lock)\n" +
	"    return False\n" +
	"\n" +
	"os.makedirs(os.path.dirname(marker),exist_ok=True)\n" +
	"try:fd=os.open(marker+'.lock',os.O_RDWR|os.O_CREAT|os.O_CLOEXEC,0o644)\n" +
	"except PermissionError:fd=os.open(marker+'.lock',os.O_RDONLY|os.O_CLOEXEC)\n" +
	"verdict='UNCERTAIN'\n" +
	"if hold(fd):\n" +
	"    raw=read_marker();verdict=marker_verdict(raw)\n" +
	"    if verdict=='CLEAR' and raw is not None:\n" +
	"        if checkout_held():verdict='HELD'\n" +
	"        else:\n" +
	"            try:os.unlink(marker)\n" +
	"            except FileNotFoundError:pass\n" +
	"if payload is None or verdict!='CLEAR':\n" +
	"    print(verdict,file=sys.stderr if payload else sys.stdout);sys.exit(75 if payload else 0)\n" +
	"sys.exit(subprocess.run(['sh','-c',payload,'icebath-update-mutex',str(fd)],pass_fds=(fd,)).returncode)\n"

// PowerShell marker verdict over .NET process facts for Windows remotes
var WindowsMarkerJudgePs = "function Get-MarkerProcessFacts([int]$ownerId){\n" +
	"if($ownerId -eq $PID){return @{Alive=$false}}\n" +
	"try{$proc=[Diagnostics.Process]::GetProcessById($ownerId)}catch [ArgumentException]{return @{Alive=$false}}\n" +
	"try{if($proc.HasExited){return @{Alive=$false}};return @{Alive=$true;Ct=([DateTimeOffset]$proc.StartTime).ToUnixTimeMilliseconds()/1000.0}}catch{return @{Alive=$true}}finally{$proc.Dispose()}\n" +
	"}\n" +
	"function Get-MarkerNow{[DateTimeOffset]::UtcNow.ToUnixTimeSeconds()}\n" +
	"function Test-MarkerIdentity($ownerId,$ct,$started,$now){\n" +
	"if($ownerId -eq 0 -or $ownerId -gt [int]::MaxValue){return $false}\n" +
	"$facts=Get-MarkerProcessFacts $ownerId\n" +
	"if(-not $facts.Alive){return $false}\n" +
	"if($null -eq $ct -or $null -eq $facts.Ct){return ($now-[double]$started) -le " +
	jsNumber(backend.V1MaxAgeSeconds) +
	"}\n" +
	"return [Math]::Abs($ct-$facts.Ct) -le " +
	jsNumber(backend.CreateTimeToleranceSecs) +
	"\n" +
	"}\n" +
	"function Get-MarkerCt([string]$digits){$value=0.0;if([double]::TryParse($digits,[Globalization.NumberStyles]::AllowDecimalPoint,[Globalization.CultureInfo]::InvariantCulture,[ref]$value)){return $value};return [double]::PositiveInfinity}\n" +
	"function Get-MarkerVerdict([string]$text){\n" +
	"$lines=@(($text -replace \"^\\uFEFF\",\"\") -split \"`n\" | ForEach-Object {($_ -replace \"`r$\",\"\").Trim([char[]]\" `t\")})\n" +
	"$style=[Globalization.NumberStyles]::None;$culture=[Globalization.CultureInfo]::InvariantCulture;[uint32]$ownerId=0;[uint64]$started=0\n" +
	"if($lines.Count -lt 2 -or $lines[0] -cnotmatch \"^[0-9]+$\" -or $lines[1] -cnotmatch \"^[0-9]+$\" -or -not [uint32]::TryParse($lines[0],$style,$culture,[ref]$ownerId) -or -not [uint64]::TryParse($lines[1],$style,$culture,[ref]$started)){return \"UNCERTAIN\"}\n" +
	"$ct=$null;if($lines.Count -gt 2 -and $lines[2] -cmatch \"^ct:([0-9]+(\\.[0-9]+)?)$\"){$ct=Get-MarkerCt $Matches[1]}\n" +
	"$ids=@(,@($ownerId,$ct))\n" +
	"foreach($line in @($lines | Select-Object -Skip 3)){[uint32]$delegateId=0;if($line -cmatch \"^delegate:([0-9]+) ct:([0-9]+(\\.[0-9]+)?)$\" -and [uint32]::TryParse($Matches[1],$style,$culture,[ref]$delegateId)){$ids+=,@($delegateId,(Get-MarkerCt $Matches[2]));break}}\n" +
	"$now=Get-MarkerNow\n" +
	"foreach($id in $ids){if(Test-MarkerIdentity $id[0] $id[1] $started $now){return \"LIVE:$($id[0])\"}}\n" +
	"return \"CLEAR\"\n" +
	"}"

// PowerShell test of a held checkout update kernel lock for Windows remotes
var WindowsCheckoutLockPs = "function Get-CheckoutLockPath([string]$root){\n" +
	"$plain=[IO.Path]::Combine($root,\".icebath-update.lock\");$dot=[IO.Path]::Combine($root,\".git\");$gitdir=$null\n" +
	"try{\n" +
	"if([IO.Directory]::Exists($dot)){$gitdir=$dot}elseif([IO.File]::Exists($dot)){$text=[IO.File]::ReadAllText($dot).Trim();if($text.StartsWith(\"gitdir:\")){$gitdir=[IO.Path]::Combine($root,$text.Substring(7).Trim())}}\n" +
	"if($gitdir){$common=[IO.Path]::Combine($gitdir,\"commondir\");if([IO.File]::Exists($common)){$gitdir=[IO.Path]::Combine($gitdir,[IO.File]::ReadAllText($common).Trim())}}\n" +
	"}catch{return $plain}\n" +
	"if(-not $gitdir){return $plain}\n" +
	"return [IO.Path]::GetFullPath([IO.Path]::Combine($gitdir,\"hermes-update.lock\"))\n" +
	"}\n" +
	"function Test-CheckoutLockHeld([object[]]$roots){\n" +
	"foreach($root in $roots){\n" +
	"if([string]::IsNullOrWhiteSpace([string]$root)){continue}\n" +
	"$lockFile=$null\n" +
	"try{$lockFile=[IO.File]::Open((Get-CheckoutLockPath ([string]$root)),[IO.FileMode]::Open,[IO.FileAccess]::Read,[IO.FileShare]::ReadWrite)}catch [IO.FileNotFoundException],[IO.DirectoryNotFoundException]{continue}catch{return $true}\n" +
	"try{$lockFile.Lock(1048576,17);$lockFile.Unlock(1048576,17)}catch{return $true}finally{$lockFile.Dispose()}\n" +
	"}\n" +
	"return $false\n" +
	"}"
