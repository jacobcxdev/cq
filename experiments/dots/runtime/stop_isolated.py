"""Stop only the verified isolated app, never the system instance."""
import json,os,signal
import isolated_native as native
import settings

def main():
    if settings.INTEGRATION_MODE == 'native':
        raise ValueError('Quit the normal desktop app before stopping Dots; CQ will not interrupt its tasks')
    port=json.loads((native.ROOT/'launch.json').read_text())['debug_port']
    if type(port) is not int or not 1024<port<65536:raise ValueError('Invalid isolated debugger port')
    pid=native.isolated_pid(port)
    os.kill(pid,signal.SIGTERM)
    print(json.dumps({'verified_isolated_main_stopped':pid}))

if __name__=='__main__':main()
