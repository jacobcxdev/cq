import test_support
import contextlib,io,json,pathlib,runpy,tempfile,unittest,subprocess,sys,os
from unittest.mock import Mock,patch
import settings

class LaunchTests(unittest.TestCase):
    def native_launch(self,root,arguments=(),environment=None,processes='',default_home=False):
        (root/'port').write_text('12345')
        (root/'realtime-port').write_text('12346')
        (root/'leaf-cert.pem').write_text('local certificate')
        def output(command,**kwargs):
            return processes if command[0]=='/bin/ps' else b'public key'
        environment={**({} if default_home else {'CODEX_HOME':str(root/'system')}),**(environment or {})}
        system=root/('.codex' if default_home else 'system')/'auth.json'
        with patch.object(settings,'ROOT',root),patch.object(settings,'SYSTEM',system),patch.object(settings,'INTEGRATION_MODE','native',create=True),patch.object(settings,'validate_runtime_paths'),patch.dict(os.environ,environment),patch('pathlib.Path.home',return_value=root),patch('subprocess.check_output',side_effect=output),patch('subprocess.run',return_value=Mock(stdout=b'der')) as run,patch('sys.argv',['launch.py',*arguments]),patch('native_debug.identity'),patch('subprocess.Popen') as spawn:
            runpy.run_path(str(pathlib.Path(__file__).with_name('launch.py')))
        return [*spawn.call_args.args[0],*[key+'='+value for key,value in spawn.call_args.kwargs['env'].items()]]

    def test_native_uses_normal_profile_and_preserves_updater(self):
        with tempfile.TemporaryDirectory() as directory:
            root=pathlib.Path(directory);root.chmod(0o700)
            with patch.dict(os.environ,{},clear=True):
                os.environ['CQ_DOTS_CONFIG']=str(root/'settings.json')
                command=self.native_launch(root,('--cloud-stdio','--computer-proxy'))
            self.assertEqual(command[:2],[str(root/'venv/bin/python'),'-u'])
            self.assertNotIn('-n',command)
            self.assertIn('CODEX_HOME='+str(root/'system'),command)
            self.assertIn('--cq-dots-runtime='+str(root.resolve()),command)
            self.assertFalse(any(value.startswith('--remote-debugging-') for value in command))
            self.assertIn('--proxy-server=http://127.0.0.1:12346',command)
            self.assertEqual(json.loads((root/'launch.json').read_text())['debug_transport'],'pipe')
            self.assertTrue(any(value.startswith('--ignore-certificate-errors-spki-list=') for value in command))
            self.assertFalse(any(value.startswith('--user-data-dir=') or value.startswith('CODEX_ELECTRON_USER_DATA_PATH=') or value.startswith('CODEX_SPARKLE_ENABLED=') or value.startswith('CODEX_TPP_LOCAL_EXECUTOR_CLI_PATH=') for value in command))
            self.assertFalse((root/'native-secondary-executor.enabled').exists())

    def test_native_keeps_path_bypass_and_public_custom_ca_trust(self):
        import ssl
        with tempfile.TemporaryDirectory() as directory:
            root=pathlib.Path(directory);root.chmod(0o700)
            extra=root/'extra.pem';extra.write_text('custom certificate')
            public=pathlib.Path(ssl.get_default_verify_paths().openssl_cafile).read_text()
            command=self.native_launch(root,environment={'PATH':'/custom/bin:/usr/bin',
                'NO_PROXY':'internal.example,localhost','NODE_EXTRA_CA_CERTS':str(extra)})
            self.assertIn('PATH=/custom/bin:/usr/bin',command)
            self.assertIn('NO_PROXY=internal.example,localhost,127.0.0.1',command)
            self.assertIn('SSL_CERT_FILE='+str(root/'native-trust.pem'),command)
            self.assertIn('NODE_EXTRA_CA_CERTS='+str(root/'native-extra-ca.pem'),command)
            bundle=(root/'native-trust.pem').read_text()
            self.assertIn(public,bundle)
            self.assertIn('custom certificate',bundle)
            self.assertIn('local certificate',bundle)
            self.assertEqual((root/'native-trust.pem').stat().st_mode&0o777,0o600)
            self.assertEqual((root/'native-extra-ca.pem').read_text(),'custom certificate\nlocal certificate')

    def test_native_default_home_does_not_add_a_home_override(self):
        with tempfile.TemporaryDirectory() as directory:
            root=pathlib.Path(directory);root.chmod(0o700)
            with patch.dict(os.environ,{'CQ_DOTS_CONFIG':str(root/'settings.json')},clear=True):
                command=self.native_launch(root,default_home=True)
            self.assertFalse(any(value.startswith('CODEX_HOME=') for value in command))

    def test_native_ignores_other_users_and_helper_processes(self):
        with tempfile.TemporaryDirectory() as directory:
            root=pathlib.Path(directory);root.chmod(0o700)
            executable=str(settings.APP/'Contents/MacOS/ChatGPT')
            processes='\n'.join([str(os.getuid()+1)+' '+executable,
                                 str(os.getuid())+' '+executable+'Helper',
                                 str(os.getuid())+' /bin/echo '+executable])
            command=self.native_launch(root,processes=processes)
            self.assertIn('--cq-dots-runtime='+str(root.resolve()),command)

    def test_native_requires_running_official_app_to_be_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            root=pathlib.Path(directory);root.chmod(0o700)
            process=str(os.getuid())+' '+str(settings.APP/'Contents/MacOS/ChatGPT')+' --some-flag\n'
            with patch('socket.socket') as socket,patch('subprocess.run') as run:
                with self.assertRaisesRegex(ValueError,'restart required'):
                    self.native_launch(root,processes=process)
            socket.assert_not_called();run.assert_not_called()
            self.assertFalse((root/'launch.json').exists())

    def test_native_rejects_another_codex_home_and_alternate_profile(self):
        for environment in ({'CODEX_HOME':'/another/home'},{'CODEX_ELECTRON_USER_DATA_PATH':'/another/profile'}):
            with self.subTest(environment=environment),tempfile.TemporaryDirectory() as directory:
                root=pathlib.Path(directory);root.chmod(0o700)
                with patch('socket.socket') as socket:
                    with self.assertRaisesRegex(ValueError,'normal'):
                        self.native_launch(root,environment=environment)
                socket.assert_not_called()
                self.assertFalse((root/'launch.json').exists())

    def test_native_rejects_research_executor_before_launch(self):
        with tempfile.TemporaryDirectory() as directory:
            root=pathlib.Path(directory);root.chmod(0o700)
            with patch('socket.socket') as socket:
                with self.assertRaisesRegex(ValueError,'research executor'):
                    self.native_launch(root,('--native-null-executor',))
            socket.assert_not_called()
            self.assertFalse((root/'launch.json').exists())

    def test_prepare_native_returns_only_embedded_plan_without_launching(self):
        with tempfile.TemporaryDirectory() as directory:
            root=pathlib.Path(directory);root.chmod(0o700)
            (root/'port').write_text('8000')
            (root/'realtime-port').write_text('12346')
            (root/'leaf-cert.pem').write_text('local certificate')
            output=io.StringIO()
            environment={'CQ_DOTS_CONFIG':str(root/'settings.json'),'CODEX_HOME':str(root/'system')}
            with patch.object(settings,'ROOT',root),patch.object(settings,'SYSTEM',root/'system/auth.json'),patch.object(settings,'INTEGRATION_MODE','native'),patch.object(settings,'validate_runtime_paths'),patch.dict(os.environ,environment),patch('subprocess.check_output',return_value=b'public key'),patch('subprocess.run',return_value=Mock(stdout=b'der')) as run,patch('subprocess.Popen') as spawn,patch('socket.socket') as socket,patch('sys.argv',['launch.py','--prepare-native','--parent-pid=123']),contextlib.redirect_stdout(output):
                runpy.run_path(str(pathlib.Path(__file__).with_name('launch.py')))
            result=json.loads(output.getvalue())
            self.assertEqual(set(result),{'env','switches'})
            self.assertLess(len(output.getvalue().encode()),65536)
            self.assertEqual(result['switches']['proxy-server'],'http://127.0.0.1:12346')
            self.assertEqual(result['switches']['cq-dots-runtime'],str(root.resolve()))
            self.assertEqual(result['env']['CODEX_CLI_PATH'],str(root/'cloud-stdio-launch'))
            self.assertEqual(result['env']['CODEX_API_BASE_URL'],'https://localhost:8000/backend-api')
            self.assertNotIn('CODEX_ELECTRON_USER_DATA_PATH',result['env'])
            self.assertEqual(json.loads((root/'launch.json').read_text())['debug_transport'],'embedded')
            spawn.assert_not_called();socket.assert_not_called()
            self.assertEqual(run.call_count,1)

    def test_prepare_native_requires_explicit_parent_and_native_mode(self):
        for arguments,mode in [(('--prepare-native',),'native'),(('--prepare-native','--parent-pid=0'),'native'),(('--prepare-native','--parent-pid=12'),'isolated')]:
            with self.subTest(arguments=arguments,mode=mode),tempfile.TemporaryDirectory() as directory:
                root=pathlib.Path(directory);root.chmod(0o700)
                with patch.object(settings,'ROOT',root),patch.object(settings,'SYSTEM',root/'system/auth.json'),patch.object(settings,'INTEGRATION_MODE',mode),patch.object(settings,'validate_runtime_paths'),patch.dict(os.environ,{'CODEX_HOME':str(root/'system')}),patch('subprocess.Popen') as spawn,patch('sys.argv',['launch.py',*arguments]):
                    with self.assertRaises(ValueError):
                        runpy.run_path(str(pathlib.Path(__file__).with_name('launch.py')))
                spawn.assert_not_called()
                self.assertFalse((root/'launch.json').exists())

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
