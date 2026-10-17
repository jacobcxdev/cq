import test_support
import json,pathlib,runpy,tempfile,unittest,subprocess,sys,os
from unittest.mock import Mock,patch
import settings

class LaunchTests(unittest.TestCase):
    def test_private_state_and_executor_marker(self):
        with tempfile.TemporaryDirectory() as directory:
            root=pathlib.Path(directory);root.chmod(0o700)
            (root/'port').write_text('12345')
            with patch.object(settings,'ROOT',root),patch.object(settings,'validate_runtime_paths') as validate,patch('subprocess.check_output',return_value=b'public key'),patch('subprocess.run',return_value=Mock(stdout=b'der')),patch('sys.argv',['launch.py','--native-null-executor']):
                runpy.run_path(str(pathlib.Path(__file__).with_name('launch.py')))
            validate.assert_called_once()
            self.assertTrue((root/'native-secondary-executor.enabled').exists())
            self.assertEqual((root/'launch.json').stat().st_mode&0o777,0o600)
            self.assertEqual(json.loads((root/'launch.json').read_text())['proxy_port'],12345)

    def test_invalid_runtime_stops_before_writes_or_process_launch(self):
        with patch.object(settings,'validate_runtime_paths',side_effect=ValueError('unsafe')),patch('subprocess.run') as run,patch('socket.socket') as socket:
            with self.assertRaises(ValueError):runpy.run_path(str(pathlib.Path(__file__).with_name('launch.py')))
        run.assert_not_called();socket.assert_not_called()

    def test_native_cli_launchers_work_when_app_replaces_path(self):
        with tempfile.TemporaryDirectory(prefix="cq launch ' ") as directory:
            root=pathlib.Path(directory);root.chmod(0o700)
            source=root/'source';source.mkdir()
            (root/'port').write_text('12345')
            (root/'venv/bin').mkdir(parents=True)
            (root/'venv/bin/python').symlink_to(sys.executable)
            (source/'launch.py').write_text(pathlib.Path(__file__).with_name('launch.py').read_text())
            for name in ('cloud_stdio.py','native_null_executor.py'):
                (source/name).write_text('import json,sys;print(json.dumps(sys.argv[1:]))')
            with patch.object(settings,'ROOT',root),patch.object(settings,'validate_runtime_paths'),patch('subprocess.check_output',return_value=b'public key'),patch('subprocess.run',return_value=Mock(stdout=b'der')) as run,patch('sys.argv',['launch.py','--cloud-stdio','--native-null-executor']):
                runpy.run_path(str(source/'launch.py'))
            command=run.call_args.args[0]
            self.assertIn('CODEX_SPARKLE_ENABLED=false',command)
            for variable,name in [('CODEX_CLI_PATH','cloud-stdio-launch'),('CODEX_TPP_LOCAL_EXECUTOR_CLI_PATH','native-executor-launch')]:
                launcher=root/name
                self.assertIn(variable+'='+str(launcher),command)
                self.assertEqual(launcher.stat().st_mode&0o777,0o700)
                output=subprocess.check_output([str(launcher),'argument with spaces',"quote'"],env={**os.environ,'PATH':'/no-python-here'},text=True)
                self.assertEqual(json.loads(output),['argument with spaces',"quote'"])

if __name__=='__main__':unittest.main()
