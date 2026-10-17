"""Run the embedded startup bridge against an Electron stub, without an app."""
import pathlib
import shutil
import subprocess
import unittest


class NativeStartupTests(unittest.TestCase):
    def test_node_harness(self):
        node = shutil.which('node')
        if node is None:
            self.skipTest('Node.js is required for the Electron startup harness')
        source = pathlib.Path(__file__).with_suffix('.cjs')
        result = subprocess.run([node, '--test', str(source)], capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == '__main__':
    unittest.main()
