import test_support
import json,os,pathlib,tempfile,unittest
from unittest.mock import patch
import isolated_native as native
import settings

class NativeIdentityTests(unittest.TestCase):
    def test_rejects_remote_debugger_urls(self):
        for url in ['ws://evil.test:9999/devtools/page/x','ws://127.0.0.1:9998/devtools/page/x','ws://user@127.0.0.1:9999/devtools/page/x','wss://127.0.0.1:9999/devtools/page/x','ws://127.0.0.1:9999/devtools/browser/x']:
            with self.subTest(url=url),self.assertRaises(ValueError):native.debugger_url(url,9999)
        self.assertEqual(native.debugger_url('ws://127.0.0.1:9999/devtools/page/x',9999),'ws://127.0.0.1:9999/devtools/page/x')

    def test_requires_exact_executable_user_data_and_listener_owner(self):
        args=str(settings.APP/'Contents/MacOS/ChatGPT')+' --user-data-dir='+str(native.ROOT/'user-data')+' --remote-debugging-port=9999'
        valid=f'123 {os.getuid()} {args}\n'
        with patch.object(native.subprocess,'check_output',side_effect=[valid,'123\n']):
            self.assertEqual(native.isolated_pid(9999),123)
        for row in [valid.replace('--user-data-dir=','--user-data-dir=/wrong'),valid.replace('MacOS/ChatGPT','MacOS/ChatGPT Helper'),valid+valid]:
            with patch.object(native.subprocess,'check_output',return_value=row),self.assertRaises(ValueError):native.isolated_pid(9999)
        with patch.object(native.subprocess,'check_output',side_effect=[valid,'124\n']),self.assertRaises(ValueError):native.isolated_pid(9999)

    def test_accepts_only_exact_direct_isolated_helper_sharing_listener(self):
        args=str(settings.APP/'Contents/MacOS/ChatGPT')+' --user-data-dir='+str(native.ROOT/'user-data')+' --remote-debugging-port=9999'
        valid=f'123 {os.getuid()} {args}\n'
        helper=str(native.ROOT/'codex-home/computer-use/Codex Computer Use.app/Contents/MacOS/SkyComputerUseService')
        child=f'123 {os.getuid()} {helper}\n'
        with patch.object(native.subprocess,'check_output',side_effect=[valid,'123\n124\n',child]):
            self.assertEqual(native.isolated_pid(9999),123)
        for bad in [child.replace('123 ', '999 ',1),child.replace(f' {os.getuid()} ',f' {os.getuid()+1} ',1),child.replace(str(native.ROOT),'/wrong'),child.strip()+' --unexpected']:
            with self.subTest(child=bad),patch.object(native.subprocess,'check_output',side_effect=[valid,'123\n124\n',bad]),self.assertRaises(ValueError):
                native.isolated_pid(9999)

    def test_native_requires_owned_runtime_marker_and_private_pipe(self):
        app=str(settings.APP/'Contents/MacOS/ChatGPT')
        marker='--cq-dots-runtime='+str(native.ROOT)
        args=app+' '+marker+' --remote-debugging-pipe'
        valid=f'123 {os.getuid()} {args}\n'
        broker=str(native.ROOT/'venv/bin/python')+' -u '+str(native.ROOT/'native_debug.py')
        with patch.object(settings,'INTEGRATION_MODE','native'), \
             patch('native_debug.identity',return_value={'app_pid':123,'broker_pid':122}):
            with patch.object(native.subprocess,'check_output',side_effect=[valid,'122',f'{os.getuid()} {broker}']):
                self.assertEqual(native.isolated_pid(),123)
            for row in (valid.replace(marker,''),valid.replace(marker,marker+'-other'),
                        valid.replace('--remote-debugging-pipe','--remote-debugging-port=9999'),
                        valid+valid,valid.replace(' --remote-debugging-pipe',' --user-data-dir=/other --remote-debugging-pipe')):
                with self.subTest(row=row),patch.object(native.subprocess,'check_output',return_value=row):
                    with self.assertRaises(ValueError):native.isolated_pid()
            for parent,identity in [('999',f'{os.getuid()} {broker}'),('122',f'{os.getuid()+1} {broker}'),
                                    ('122',f'{os.getuid()} /wrong/broker')]:
                with patch.object(native.subprocess,'check_output',side_effect=[valid,parent,identity]):
                    with self.assertRaises(ValueError):native.isolated_pid()

    def test_native_accepted_only_configured_framework_broker(self):
        with tempfile.TemporaryDirectory() as directory:
            base=pathlib.Path(directory).resolve()
            root=base/'runtime';(root/'venv/bin').mkdir(parents=True)
            version=base/'Cellar/python@3.14/3.14.8/Frameworks/Python.framework/Versions/3.14'
            interpreter=version/'bin/python3.14';interpreter.parent.mkdir(parents=True)
            interpreter.write_text('framework launcher fixture')
            executable=version/'Resources/Python.app/Contents/MacOS/Python'
            executable.parent.mkdir(parents=True);executable.write_text('framework executable fixture')
            (root/'venv/bin/python').symlink_to(interpreter)
            valid=f'123 {os.getuid()} {settings.APP}/Contents/MacOS/ChatGPT --cq-dots-runtime={root} --remote-debugging-pipe\n'
            broker=f'{executable} -u {root}/native_debug.py'
            with patch.object(native,'ROOT',root),patch.object(settings,'INTEGRATION_MODE','native'), \
                 patch('native_debug.identity',return_value={'app_pid':123,'broker_pid':122}):
                with patch.object(native.subprocess,'check_output',side_effect=[valid,'122',f'{os.getuid()} {broker}']):
                    self.assertEqual(native.isolated_pid(),123)
                for parent,identity in [('999',f'{os.getuid()} {broker}'),
                        ('122',f'{os.getuid()+1} {broker}'),
                        ('122',f'{os.getuid()} /other/Frameworks/Python.framework/Versions/3.14/Resources/Python.app/Contents/MacOS/Python -u {root}/native_debug.py'),
                        ('122',f'{os.getuid()} {executable} -c {root}/native_debug.py'),
                        ('122',f'{os.getuid()} {executable} -u {base}/native_debug.py')]:
                    with self.subTest(parent=parent,identity=identity),patch.object(native.subprocess,'check_output',side_effect=[valid,parent,identity]):
                        with self.assertRaises(ValueError):native.isolated_pid()

    def test_process_inventory_skipped_malformed_ids_and_negative_system_uid(self):
        args=str(settings.APP/'Contents/MacOS/ChatGPT')+' --user-data-dir='+str(native.ROOT/'user-data')+' --remote-debugging-port=9999'
        invalid=['not-a-pid '+str(os.getuid())+' '+args,'777 invalid '+args,'778 -2 /usr/libexec/system-daemon']
        rows='\n'.join(invalid+[f'123 {os.getuid()} {args}'])+'\n'
        with patch.object(native.subprocess,'check_output',side_effect=[rows,'123\n']):
            self.assertEqual(native.isolated_pid(9999),123)

if __name__=='__main__':unittest.main()
