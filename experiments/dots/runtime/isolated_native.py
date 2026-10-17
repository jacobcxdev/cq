"""Call stock native API only inside the verified isolated renderer."""
import hashlib,json,os,pathlib,shlex,subprocess,urllib.parse,urllib.request
from websockets.sync.client import connect
import settings
ROOT=settings.ROOT
_verified_app=None

def app_check():
    global _verified_app
    archive=settings.APP/'Contents/Resources/app.asar'
    info=archive.stat(); identity=(info.st_dev,info.st_ino,info.st_size,info.st_mtime_ns)
    if identity!=_verified_app:
        if hashlib.sha256(archive.read_bytes()).hexdigest()!=settings.ASAR_SHA256:
            raise ValueError('Native renderer differs from qualified build')
        _verified_app=identity

def isolated_pid(port):
    rows=subprocess.check_output(['/bin/ps','-axo','pid=,uid=,command='],text=True).splitlines()
    expected=str(settings.APP/'Contents/MacOS/ChatGPT')
    matches=[]
    for row in rows:
        values=row.strip().split(maxsplit=2)
        if len(values)!=3:continue
        try:args=shlex.split(values[2])
        except ValueError:continue
        if (int(values[1])==os.getuid() and args and args[0]==expected
                and [a for a in args if a.startswith('--user-data-dir=')]==['--user-data-dir='+str(ROOT/'user-data')]
                and [a for a in args if a.startswith('--remote-debugging-port=')]==['--remote-debugging-port='+str(port)]):
            matches.append(int(values[0]))
    if len(matches)!=1:raise ValueError('Exact isolated process unavailable')
    listeners=subprocess.check_output(['/usr/sbin/lsof','-t','-a','-iTCP:'+str(port),'-sTCP:LISTEN'],text=True)
    owners={int(x) for x in listeners.split()}
    if matches[0] not in owners:
        raise ValueError('Isolated debugger listener ownership changed')
    # Stock computer-use helper inherits this socket from its direct app parent.
    helper=str(ROOT/'codex-home/computer-use/Codex Computer Use.app/Contents/MacOS/SkyComputerUseService')
    for pid in owners-{matches[0]}:
        child=subprocess.check_output(['/bin/ps','-p',str(pid),'-o','ppid=,uid=,command='],text=True).strip().split(maxsplit=2)
        if (len(child)!=3 or child[0]!=str(matches[0]) or child[1]!=str(os.getuid())
                or child[2]!=helper):
            raise ValueError('Isolated debugger listener ownership changed')
    return matches[0]

def debugger_url(value,port):
    url=urllib.parse.urlsplit(value)
    if (url.scheme!='ws' or url.hostname!='127.0.0.1' or url.port!=port
            or url.username is not None or url.password is not None or url.fragment
            or not url.path.startswith('/devtools/page/')):
        raise ValueError('Isolated debugger URL invalid')
    return value

def evaluate(expression):
    app_check()
    launch=json.loads((ROOT/'launch.json').read_text());port=launch['debug_port']
    if type(port) is not int or not 1024<port<65536:raise ValueError('Invalid isolated debugger port')
    isolated_pid(port)
    with urllib.request.urlopen(f'http://127.0.0.1:{port}/json/list',timeout=5) as response:
        data=response.read(1024*1024+1)
    if len(data)>1024*1024:raise ValueError('Debugger target response too large')
    targets=json.loads(data)
    matches=[t for t in targets if t.get('type')=='page' and t.get('url')=='app://-/index.html']
    if len(matches)!=1:raise ValueError('Isolated renderer selection ambiguous')
    with connect(debugger_url(matches[0]['webSocketDebuggerUrl'],port),proxy=None,open_timeout=5,max_size=2*1024*1024) as ws:
        ws.send(json.dumps({'id':1,'method':'Runtime.evaluate','params':{'expression':expression,'awaitPromise':True,'returnByValue':True}}))
        while True:
            value=json.loads(ws.recv(timeout=35))
            if value.get('id')==1:
                result=value.get('result',{})
                if value.get('error') or result.get('exceptionDetails'):raise ValueError('Native API invocation failed')
                return result['result'].get('value')

if __name__=='__main__':
    expression="(async()=>{const m=await import('app://-/assets/"+settings.MODULE_ASSET+"');return {nativeApi:typeof m.Ymr?.safeGet==='function'};})()"
    print(json.dumps(evaluate(expression)))
