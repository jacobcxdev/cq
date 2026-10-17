import pathlib, json, socket, subprocess, hashlib, base64, sys, os, shlex, ssl
import settings
p=settings.ROOT
settings.validate_runtime_paths()
native=getattr(settings,'INTEGRATION_MODE','research')=='native'
prepare_native='--prepare-native' in sys.argv
if prepare_native and not native:
    raise ValueError('Embedded startup requires native integration mode')
if native:
    if '--native-null-executor' in sys.argv:
        raise ValueError('Native Dots launch cannot use the research executor')
    inherited_home=os.environ.get('CODEX_HOME')
    normal_home=pathlib.Path(inherited_home) if inherited_home else pathlib.Path.home()/'.codex'
    if (inherited_home=='' or not normal_home.is_absolute()
            or normal_home.resolve()!=settings.SYSTEM.parent.resolve()):
        raise ValueError('Native Dots launch requires the configured normal CODEX_HOME')
    if 'CODEX_ELECTRON_USER_DATA_PATH' in os.environ:
        raise ValueError('Native Dots launch requires the normal desktop profile')
    if prepare_native:
        parents=[value.split('=',1)[1] for value in sys.argv if value.startswith('--parent-pid=')]
        if len(parents)!=1 or not parents[0].isdigit() or int(parents[0])<=0:
            raise ValueError('Embedded startup requires its verified parent PID')
    else:
        expected=str(settings.APP/'Contents/MacOS/ChatGPT')
        rows=subprocess.check_output(['/bin/ps','-axo','uid=,command='],text=True).splitlines()
        for row in rows:
            values=row.strip().split(maxsplit=1)
            if (len(values)==2 and values[0]==str(os.getuid())
                    and (values[1]==expected or values[1].startswith(expected+' '))):
                raise ValueError('Codex restart required: quit the official desktop app, then start Dots again; CQ will not stop it')
source=pathlib.Path(__file__).resolve().parent
port=int((p/'port').read_text())
debug=None
if not native:
    s=socket.socket(); s.bind(('127.0.0.1',settings.DEBUG_PORT)); debug=s.getsockname()[1]; s.close()
def private_state(name,value,mode=0o600):
    path=p/name
    temporary=path.with_name(path.name+'.launch-tmp')
    fd=os.open(temporary,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,mode)
    try:
        with os.fdopen(fd,'w') as stream:
            stream.write(value);stream.flush();os.fsync(stream.fileno())
        temporary.replace(path)
    finally:temporary.unlink(missing_ok=True)
def cli_launcher(name,script):
    # Native app replaces PATH; env-python shebangs do not preserve this venv.
    command='#!/bin/sh\nexec '+shlex.quote(str(p/'venv/bin/python'))+' '+shlex.quote(str(source/script))+' "$@"\n'
    private_state(name,command,0o700)
    return str(p/name)
private_state('launch.json',json.dumps({'debug_transport':'embedded' if prepare_native else ('pipe' if native else 'tcp'),
    'debug_port':debug,'proxy_port':port,'catalogue_revision':settings.catalogue_revision()}))
pub=subprocess.check_output(['openssl','x509','-in',str(p/'leaf-cert.pem'),'-pubkey','-noout'])
der=subprocess.run(['openssl','pkey','-pubin','-outform','DER'],input=pub,capture_output=True,check=True).stdout
spki=base64.b64encode(hashlib.sha256(der).digest()).decode()
cmd=[] if native else ['/usr/bin/open','-n']
env={'CQ_DOTS_CONFIG':str(pathlib.Path(os.environ['CQ_DOTS_CONFIG']).resolve()),'PATH':str(p/'venv/bin')+os.pathsep+os.environ.get('PATH','/usr/bin:/bin'),'CODEX_API_BASE_URL':f'https://localhost:{port}/backend-api','CODEX_APP_SERVER_CHATGPT_BASE_URL':f'https://localhost:{port}/backend-api','NODE_EXTRA_CA_CERTS':str(p/'probe-cert.pem'),'SSL_CERT_FILE':str(p/'probe-cert.pem'),'NO_PROXY':'localhost,127.0.0.1'}
if native:
    # Native tools inherit this environment. Keep public/custom roots and PATH,
    # adding only our exact local certificate rather than replacing HTTPS trust.
    ca_paths=ssl.get_default_verify_paths()
    public_ca=ca_paths.cafile or ca_paths.openssl_cafile
    if not public_ca:raise ValueError('Public certificate bundle unavailable')
    public_pem=pathlib.Path(public_ca).read_text()
    extra_pem=(pathlib.Path(os.environ['NODE_EXTRA_CA_CERTS']).read_text()
               if os.environ.get('NODE_EXTRA_CA_CERTS') else '')
    local_pem=(p/'leaf-cert.pem').read_text()
    private_state('native-trust.pem',public_pem+'\n'+extra_pem+'\n'+local_pem)
    private_state('native-extra-ca.pem',extra_pem+'\n'+local_pem)
    env.update({'PATH':os.environ.get('PATH','/usr/bin:/bin'),
                'SSL_CERT_FILE':str(p/'native-trust.pem'),
                'NODE_EXTRA_CA_CERTS':str(p/'native-extra-ca.pem'),
                'NO_PROXY':','.join(dict.fromkeys(filter(None,
                    [*os.environ.get('NO_PROXY','').split(','),'localhost','127.0.0.1'])))})
    if inherited_home is not None:env['CODEX_HOME']=inherited_home
else:
    env.update({'CODEX_HOME':str(p/'codex-home'),'CODEX_ELECTRON_USER_DATA_PATH':str(p/'user-data')})
if '--native-null-executor' in sys.argv:
    private_state('native-secondary-executor.enabled','enabled\n')
    env['CODEX_TPP_LOCAL_EXECUTOR_CLI_PATH']=cli_launcher('native-executor-launch','native_null_executor.py')
else:
    (p/'native-secondary-executor.enabled').unlink(missing_ok=True)
if '--cloud-stdio' in sys.argv or prepare_native:
    env.update({'CODEX_APP_SERVER_FORCE_CLI':'1','CODEX_CLI_PATH':cli_launcher('cloud-stdio-launch','cloud_stdio.py')})
elif (p/'connect-port').exists():
    env.update({'NODE_USE_ENV_PROXY':'1','HTTPS_PROXY':'http://127.0.0.1:'+(p/'connect-port').read_text(),'HTTP_PROXY':'http://127.0.0.1:'+(p/'connect-port').read_text()})
if not native:
    for k,v in env.items():cmd+=['--env',k+'='+v]
    cmd+=['--env','CODEX_SPARKLE_ENABLED=false',str(settings.APP),'--args']
cmd+=[('--cq-dots-runtime='+str(p.resolve())) if native else ('--user-data-dir='+str(p/'user-data'))]
if not native:cmd+=['--remote-debugging-port='+str(debug),'--remote-debugging-address=127.0.0.1']
cmd+=['--ignore-certificate-errors-spki-list='+spki]
if '--computer-proxy' in sys.argv or prepare_native:
    cmd+=['--proxy-server=http://127.0.0.1:'+(p/'realtime-port').read_text(),
          '--proxy-bypass-list=localhost;127.0.0.1']
if prepare_native:
    result={'env':env,'switches':{value[2:].split('=',1)[0]:value.split('=',1)[1] for value in cmd}}
    encoded=json.dumps(result,separators=(',',':'))
    if len(encoded.encode())>65536:raise ValueError('Embedded startup configuration exceeds bound')
    print(encoded)
elif native:
    # Spawn the signed app with its normal profile. Private inherited CDP pipes
    # replace the research debugger TCP port; no global LaunchServices edits.
    import native_debug, time
    native_debug._root(p)
    log=p/'native-debug.log'
    fd=os.open(log,os.O_WRONLY|os.O_CREAT|os.O_APPEND|os.O_NOFOLLOW,0o600)
    os.fchmod(fd,0o600)
    with os.fdopen(fd,'ab') as stream:
        broker=subprocess.Popen([str(p/'venv/bin/python'),'-u',str(source/'native_debug.py'),*cmd],
            cwd=p,env={**os.environ,**env},stdout=stream,stderr=stream,start_new_session=True)
    deadline=time.monotonic()+10
    while True:
        try:
            native_debug.identity(p)
            break
        except (OSError,ValueError):
            if broker.poll() is not None or time.monotonic()>=deadline:
                raise ValueError('Native private renderer bridge failed to start') from None
            time.sleep(0.1)
else:subprocess.run(cmd,check=True)
if not prepare_native:
    print(('normal-profile' if native else 'isolated')+' launch with exact probe certificate exception',{'debug_port':debug,'proxy_port':port, 'catalogue_revision':settings.catalogue_revision()})
