package connections

// Observation program printing the marker, intent, exit code, receipt and readiness of one correlation as JSON
var RemoteUpdateObservationPy = "import ctypes,json,os,re,sys\n" +
	"from pathlib import Path\n" +
	RemoteMarkerJudgePy +
	"\n" +
	"\n" +
	"home=Path(os.path.expanduser(sys.argv[1]))\n" +
	"correlation=sys.argv[2]\n" +
	"# The update marker and the receipt store are install-wide even when the launcher\n" +
	"# was invoked with a named profile home (<root>/profiles/<name>): the CLI writes\n" +
	"# receipts under the root home. Correlated status/ready/intent stay under the\n" +
	"# launch home, matching their writers.\n" +
	"profile_parent=home.parent.name\n" +
	"is_profile_home=(profile_parent.lower()=='profiles') if os.name=='nt' else (profile_parent=='profiles')\n" +
	"install_root=home.parent.parent if is_profile_home else home\n" +
	"marker_path=install_root/'.icebath-update-in-progress'\n" +
	"status_path=home/('.update_exit_code.'+correlation)\n" +
	"ready_path=home/('.update_coordinator_ready.'+correlation)\n" +
	"intent_path=home/('.update_launch_intent.'+correlation)\n" +
	"\n" +
	"def pid_alive(pid):\n" +
	"    if os.name!='nt':\n" +
	"        try:\n" +
	"            os.kill(pid,0);return True\n" +
	"        except ProcessLookupError:return False\n" +
	"        except PermissionError:return True\n" +
	"        except OSError:return None\n" +
	"    try:\n" +
	"        from ctypes import wintypes\n" +
	"        kernel=ctypes.WinDLL('kernel32',use_last_error=True)\n" +
	"        kernel.OpenProcess.argtypes=[wintypes.DWORD,wintypes.BOOL,wintypes.DWORD]\n" +
	"        kernel.OpenProcess.restype=wintypes.HANDLE\n" +
	"        kernel.GetExitCodeProcess.argtypes=[wintypes.HANDLE,ctypes.POINTER(wintypes.DWORD)]\n" +
	"        kernel.GetExitCodeProcess.restype=wintypes.BOOL\n" +
	"        kernel.CloseHandle.argtypes=[wintypes.HANDLE]\n" +
	"        handle=kernel.OpenProcess(0x1000,False,pid)\n" +
	"        if not handle:\n" +
	"            return False if ctypes.get_last_error()==87 else None\n" +
	"        try:\n" +
	"            code=wintypes.DWORD()\n" +
	"            if not kernel.GetExitCodeProcess(handle,ctypes.byref(code)):return None\n" +
	"            return code.value==259\n" +
	"        finally:kernel.CloseHandle(handle)\n" +
	"    except Exception:return None\n" +
	"\n" +
	"def process_creation(pid):\n" +
	"    if os.name!='nt':\n" +
	"        if sys.platform.startswith('linux'):\n" +
	"            try:\n" +
	"                raw=Path('/proc/'+str(pid)+'/stat').read_text(encoding='ascii')\n" +
	"                return 'linux:'+raw[raw.rfind(')')+2:].split()[19]\n" +
	"            except (OSError,UnicodeError,IndexError):return None\n" +
	"        if sys.platform=='darwin':\n" +
	"            try:\n" +
	"                import subprocess\n" +
	"                value=subprocess.check_output(['ps','-o','lstart=','-p',str(pid)],text=True).strip()\n" +
	"                return 'darwin:'+value if value else None\n" +
	"            except (OSError,subprocess.CalledProcessError):return None\n" +
	"        return None\n" +
	"    try:\n" +
	"        from ctypes import wintypes\n" +
	"        kernel=ctypes.WinDLL('kernel32',use_last_error=True)\n" +
	"        kernel.OpenProcess.argtypes=[wintypes.DWORD,wintypes.BOOL,wintypes.DWORD]\n" +
	"        kernel.OpenProcess.restype=wintypes.HANDLE\n" +
	"        kernel.GetProcessTimes.argtypes=[wintypes.HANDLE,ctypes.POINTER(wintypes.FILETIME),ctypes.POINTER(wintypes.FILETIME),ctypes.POINTER(wintypes.FILETIME),ctypes.POINTER(wintypes.FILETIME)]\n" +
	"        kernel.GetProcessTimes.restype=wintypes.BOOL\n" +
	"        kernel.CloseHandle.argtypes=[wintypes.HANDLE]\n" +
	"        handle=kernel.OpenProcess(0x1000,False,pid)\n" +
	"        if not handle:return None\n" +
	"        try:\n" +
	"            created=wintypes.FILETIME();exited=wintypes.FILETIME();kernel_time=wintypes.FILETIME();user_time=wintypes.FILETIME()\n" +
	"            if not kernel.GetProcessTimes(handle,ctypes.byref(created),ctypes.byref(exited),ctypes.byref(kernel_time),ctypes.byref(user_time)):return None\n" +
	"            value=(created.dwHighDateTime<<32)|created.dwLowDateTime\n" +
	"            return 'windows:'+str(value)\n" +
	"        finally:kernel.CloseHandle(handle)\n" +
	"    except Exception:return None\n" +
	"\n" +
	"def marker_state():\n" +
	"    try:raw=marker_path.read_bytes()\n" +
	"    except FileNotFoundError:return {'state':'absent'}\n" +
	"    except OSError:return {'state':'unavailable'}\n" +
	"    verdict=marker_verdict(raw)\n" +
	"    if verdict=='UNCERTAIN':return {'state':'malformed'}\n" +
	"    if verdict=='CLEAR':return {'state':'dead'}\n" +
	"    return {'state':'live','pid':int(verdict[5:])}\n" +
	"\n" +
	"def terminal_code():\n" +
	"    try:raw=status_path.read_bytes()\n" +
	"    except FileNotFoundError:return None\n" +
	"    except OSError:return 'unavailable'\n" +
	"    if not re.fullmatch(rb'[0-9]+',raw):return 'malformed'\n" +
	"    try:return int(raw)\n" +
	"    except ValueError:return 'malformed'\n" +
	"\n" +
	"def receipt():\n" +
	"    directory=install_root/'logs'/'update_receipts'\n" +
	"    try:paths=sorted(directory.glob('update_*.json'),key=lambda p:p.stat().st_mtime_ns,reverse=True)\n" +
	"    except OSError:return None\n" +
	"    for path in paths:\n" +
	"        try:payload=json.loads(path.read_text(encoding='utf-8'))\n" +
	"        except (OSError,UnicodeError,ValueError):continue\n" +
	"        if not isinstance(payload,dict) or payload.get('correlation_id')!=correlation:continue\n" +
	"        if payload.get('outcome')=='running' or not payload.get('finished_at'):continue\n" +
	"        pre=payload.get('pre_update') if isinstance(payload.get('pre_update'),dict) else {}\n" +
	"        post=payload.get('post_update') if isinstance(payload.get('post_update'),dict) else {}\n" +
	"        action=payload.get('user_action')\n" +
	"        return {\n" +
	"            'correlationId':correlation,'outcome':str(payload.get('outcome') or 'unknown'),\n" +
	"            'startedAt':payload.get('started_at'),'finishedAt':payload.get('finished_at'),\n" +
	"            'preSha':pre.get('sha'),'postSha':post.get('sha'),\n" +
	"            'preVersion':pre.get('version'),'postVersion':post.get('version'),\n" +
	"            'stopReason':payload.get('stop_reason'),\n" +
	"            'followups':[{'step':str(f.get('step')),'reason':str(f.get('reason') or '')} for f in payload.get('followups') or [] if isinstance(f,dict)],\n" +
	"            'userAction':{'step':str(action.get('step')),'reason':str(action.get('reason') or '')} if isinstance(action,dict) else None,\n" +
	"        }\n" +
	"    return None\n" +
	"\n" +
	"def ready():\n" +
	"    try:payload=json.loads(ready_path.read_text(encoding='utf-8'))\n" +
	"    except (FileNotFoundError,OSError,UnicodeError,ValueError):return None\n" +
	"    if not isinstance(payload,dict) or payload.get('correlation_id')!=correlation:return None\n" +
	"    pid=payload.get('pid')\n" +
	"    return {'correlationId':correlation,'pid':pid} if isinstance(pid,int) and pid>0 else None\n" +
	"\n" +
	"def launch_intent():\n" +
	"    try:payload=json.loads(intent_path.read_text(encoding='utf-8'))\n" +
	"    except FileNotFoundError:return 'absent'\n" +
	"    except OSError:return 'unavailable'\n" +
	"    except (UnicodeError,ValueError):return 'malformed'\n" +
	"    if not isinstance(payload,dict) or payload.get('correlation')!=correlation:return 'malformed'\n" +
	"    pid=payload.get('pid');creation=payload.get('creation')\n" +
	"    if not isinstance(pid,int) or pid<1 or pid>4294967295 or not isinstance(creation,str):return 'malformed'\n" +
	"    live=pid_alive(pid)\n" +
	"    if live is None:return 'unavailable'\n" +
	"    if not live:return 'dead'\n" +
	"    current=process_creation(pid)\n" +
	"    if current is None:return 'unavailable'\n" +
	"    return 'present' if current==creation else 'dead'\n" +
	"\n" +
	"state=marker_state()\n" +
	"print(json.dumps({'marker':state['state'],'markerPid':state.get('pid'),'launchIntent':launch_intent(),'exitCode':terminal_code(),'receipt':receipt(),'coordinatorReady':ready()},separators=(',',':')))"
