"""Synthetic settings bootstrap; imported before runtime modules by offline tests."""
import atexit
import json
import os
import pathlib
import tempfile

_DIRECTORY = tempfile.TemporaryDirectory(prefix='cq-dots-test-')
atexit.register(_DIRECTORY.cleanup)
ROOT = pathlib.Path(_DIRECTORY.name)
ROOT.chmod(0o700)
CONFIG = json.loads(pathlib.Path(__file__).with_name('settings.example.json').read_text())
CONFIG['runtime_root'] = str(ROOT / 'runtime')
(ROOT / 'runtime').mkdir(mode=0o700)
CONFIG_PATH = ROOT / 'settings.json'
CONFIG_PATH.write_text(json.dumps(CONFIG))
CONFIG_PATH.chmod(0o600)
os.environ['CQ_DOTS_CONFIG'] = str(CONFIG_PATH)
