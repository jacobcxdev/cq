import pathlib, json, socket, subprocess, hashlib, base64, sys, os, shlex
import settings
p=settings.ROOT
settings.validate_runtime_paths()
source=pathlib.Path(__file__).resolve().parent
port=int((p/'port').read_text())
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
private_state('launch.json',json.dumps({'debug_port':debug,'proxy_port':port}))
pub=subprocess.check_output(['openssl','x509','-in',str(p/'leaf-cert.pem'),'-pubkey','-noout'])
der=subprocess.run(['openssl','pkey','-pubin','-outform','DER'],input=pub,capture_output=True,check=True).stdout
spki=base64.b64encode(hashlib.sha256(der).digest()).decode()
cmd=['/usr/bin/open','-n']
env={'CQ_DOTS_CONFIG':str(pathlib.Path(os.environ['CQ_DOTS_CONFIG']).resolve()),'PATH':str(p/'venv/bin')+os.pathsep+os.environ.get('PATH','/usr/bin:/bin'),'CODEX_HOME':str(p/'codex-home'),'CODEX_ELECTRON_USER_DATA_PATH':str(p/'user-data'),'CODEX_API_BASE_URL':f'https://localhost:{port}/backend-api','CODEX_APP_SERVER_CHATGPT_BASE_URL':f'https://localhost:{port}/backend-api','NODE_EXTRA_CA_CERTS':str(p/'probe-cert.pem'),'SSL_CERT_FILE':str(p/'probe-cert.pem'),'NO_PROXY':'localhost,127.0.0.1'}
if '--native-null-executor' in sys.argv:
    private_state('native-secondary-executor.enabled','enabled\n')
    env['CODEX_TPP_LOCAL_EXECUTOR_CLI_PATH']=cli_launcher('native-executor-launch','native_null_executor.py')
else:
    (p/'native-secondary-executor.enabled').unlink(missing_ok=True)
if '--cloud-stdio' in sys.argv:
    env.update({'CODEX_APP_SERVER_FORCE_CLI':'1','CODEX_CLI_PATH':cli_launcher('cloud-stdio-launch','cloud_stdio.py')})
elif (p/'connect-port').exists():
    env.update({'NODE_USE_ENV_PROXY':'1','HTTPS_PROXY':'http://127.0.0.1:'+(p/'connect-port').read_text(),'HTTP_PROXY':'http://127.0.0.1:'+(p/'connect-port').read_text()})
for k,v in env.items():cmd+=['--env',k+'='+v]
cmd+=['--env','CODEX_SPARKLE_ENABLED=false']
cmd+=[str(settings.APP),'--args','--user-data-dir='+str(p/'user-data'),'--remote-debugging-port='+str(debug),'--remote-debugging-address=127.0.0.1','--ignore-certificate-errors-spki-list='+spki]
if '--computer-proxy' in sys.argv:
    cmd+=['--proxy-server=http://127.0.0.1:'+(p/'realtime-port').read_text(),
          '--proxy-bypass-list=localhost;127.0.0.1']
subprocess.run(cmd,check=True)
print('isolated launch with exact probe certificate exception',{'debug_port':debug,'proxy_port':port})
