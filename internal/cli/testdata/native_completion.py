"""Press native Tab/Enter and report the argv received by a harmless cq function."""

import json
import os
from pathlib import Path
import pty
import select
import shlex
import signal
import sys
import tempfile
import time


shell, completion, command = sys.argv[1:]
with tempfile.TemporaryDirectory(prefix="cq-native-completion-") as directory:
    root = Path(directory)
    (root / "Équipe bleue.json").write_text("{}\n")
    (root / "service").mkdir()
    environment = {
        "HOME": directory,
        "PATH": "/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin",
        "TERM": "xterm-256color",
        "LC_ALL": "en_US.UTF-8",
        "PS1": "cq-test> ",
    }
    pid, terminal = pty.fork()
    if pid == 0:
        os.chdir(directory)
        arguments = [shell, "--noprofile", "--norc", "-i"] if shell.endswith("bash") else [shell, "-f", "-i"]
        os.execve(shell, arguments, environment)

    transcript = bytearray()

    def wait_for(marker):
        received = bytearray()
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            if select.select([terminal], [], [], 0.1)[0]:
                chunk = os.read(terminal, 65536)
                if not chunk:
                    break
                transcript.extend(chunk)
                received.extend(chunk)
                if marker in received:
                    return
        raise RuntimeError("native shell did not finish:\n" + transcript.decode(errors="replace"))

    try:
        setup = ""
        if shell.endswith("zsh"):
            setup += "autoload -Uz compinit; compinit -D; "
        setup += "source " + shlex.quote(completion) + "; "
        setup += "cq() { printf '%s\\0' \"$@\" > argv; printf '\\n__CQ_DONE__\\n'; }; "
        setup += "printf '\\n__CQ_READY__\\n'\n"
        os.write(terminal, setup.encode())
        wait_for(b"\r\n__CQ_READY__\r\n")
        os.write(terminal, command.encode() + b"\t\n")
        wait_for(b"\r\n__CQ_DONE__\r\n")
        arguments = (root / "argv").read_bytes().split(b"\0")[:-1]
        print(json.dumps([arg.decode() for arg in arguments], ensure_ascii=False))
    finally:
        os.kill(pid, signal.SIGKILL)
        os.close(terminal)
        os.waitpid(pid, 0)
