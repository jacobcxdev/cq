#!/usr/bin/env python3
"""Stock native CLI hook, owner-pinned to existing isolated secondary environment."""
import json,os,signal,sys,threading,time
import local_executor as le
def save(value):
    path=le.ROOT/'proof.json';temp=path.with_suffix('.tmp')
    temp.write_text(json.dumps(value,indent=2)+'\n');temp.chmod(0o600);temp.replace(path)

stop=threading.Event()
def request_stop(*_):stop.set()

def main():
    args=sys.argv[1:]
    proof=json.loads((le.ROOT/'proof.json').read_text())
    expected=proof.get('environment_id')
    if ('exec-server' not in args or '--environment-id' not in args
            or args[args.index('--environment-id')+1]!=expected
            or proof.get('owner_alias')!=le.ALIAS or proof.get('executor_stopped') is not True):
        raise ValueError('Native executor outside isolated reuse scope')
    owner=le.null_credential()
    if proof.get('root_thread_id')!=le.current_root(owner):raise ValueError('secondary root changed')
    le.prepare();broker=le.RegistryBroker(owner,expected);broker.start()
    process=le.launch(broker.server.server_port,expected,broker.capability)
    proof.update({'executor_stopped':False,'executor_pid':process.pid,
                  'native_executor_wrapper_pid':os.getpid(),'native_singleton_managed':True,
                  'connected_at':time.time()})
    save(proof)
    def forward(stream,target):
        try:
            for line in iter(stream.readline,b''):
                target.buffer.write(line);target.flush()
        except Exception:stop.set()
    for source,target in [(process.stdout,sys.stdout),(process.stderr,sys.stderr)]:
        threading.Thread(target=forward,args=(source,target),daemon=True).start()
    def stdin_closed():
        try:
            while sys.stdin.buffer.read(1024):pass
        finally:stop.set()
    threading.Thread(target=stdin_closed,daemon=True).start()
    for sig in [signal.SIGINT,signal.SIGTERM]:signal.signal(sig,request_stop)
    try:
        while process.poll() is None and not stop.wait(1):pass
    finally:
        if process.poll() is None:
            process.stdin.close()
            try:process.wait(timeout=10)
            except Exception:process.terminate();process.wait(timeout=5)
        broker.close();proof.update({'executor_stopped':True,'executor_exit_code':process.poll()});save(proof)

if __name__=='__main__':main()
