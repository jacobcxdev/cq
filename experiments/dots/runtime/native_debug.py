"""Private native renderer access over Chromium's inherited debugging pipe.

Chromium reads fd 3 and writes fd 4, using NUL-delimited JSON. Electron quits
when that pipe disconnects. This broker therefore keeps both descriptors open
after SIGINT, SIGTERM or an IPC failure, until the app exits on its own. SIGKILL
or an interpreter crash can still close the pipe and make Electron quit.
"""
import json
import ctypes
import os
import pathlib
import selectors
import signal
import socket
import stat
import struct
import sys
import threading
import time

import settings


MAX_FRAME = 2 * 1024 * 1024
MAX_REQUEST = 1024 * 1024
TIMEOUT = 35
SOCKET_NAME = 'native-debug.sock'
STATE_NAME = 'native-debug.json'


def _root(root=None):
    root = pathlib.Path(root) if root is not None else settings.ROOT
    info = root.lstat()
    if (not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid()
            or stat.S_IMODE(info.st_mode) != 0o700):
        raise ValueError('Native debugger directory must be owned and mode 0700')
    if len(os.fsencode(root / SOCKET_NAME)) > 103:
        raise ValueError('Native debugger socket path is too long')
    return root


def _peer_uid(connection):
    if hasattr(connection, 'getpeereid'):
        return connection.getpeereid()[0]
    if hasattr(socket, 'SO_PEERCRED'):
        return struct.unpack('3i', connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))[1]
    if sys.platform == 'darwin':
        # CPython does not expose getpeereid on every macOS build.
        getpeereid = ctypes.CDLL(None, use_errno=True).getpeereid
        getpeereid.argtypes = [ctypes.c_int, ctypes.POINTER(ctypes.c_uint), ctypes.POINTER(ctypes.c_uint)]
        uid, gid = ctypes.c_uint(), ctypes.c_uint()
        if getpeereid(connection.fileno(), ctypes.byref(uid), ctypes.byref(gid)) != 0:
            raise OSError(ctypes.get_errno(), 'Native debugger peer verification failed')
        return uid.value
    raise ValueError('Native debugger peer verification unavailable')


def _receive(connection, limit):
    value = bytearray()
    deadline = time.monotonic() + TIMEOUT + 5
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise ValueError('Native debugger frame timed out')
        connection.settimeout(remaining)
        chunk = connection.recv(min(65536, limit + 1 - len(value)))
        if not chunk:
            raise ValueError('Native debugger request incomplete')
        value.extend(chunk)
        if len(value) > limit:
            raise ValueError('Native debugger frame too large')
        if b'\n' in chunk:
            if not value.endswith(b'\n') or value.count(b'\n') != 1:
                raise ValueError('Native debugger frame invalid')
            return json.loads(value)


def _send(connection, value, limit):
    data = json.dumps(value, separators=(',', ':'), allow_nan=False).encode() + b'\n'
    if len(data) > limit:
        raise ValueError('Native debugger frame too large')
    connection.sendall(data)


class Pipe:
    """One CDP command at a time; unsolicited events are discarded."""

    def __init__(self, read_fd, write_fd):
        self.read_fd = read_fd
        self.write_fd = write_fd
        self.sequence = 0
        self.pending = None
        self.reply = None
        self.failure = False
        self.condition = threading.Condition()
        self.commands = threading.Lock()
        os.set_blocking(write_fd, False)
        self.reader = threading.Thread(target=self._read, daemon=True)
        self.reader.start()

    def _read(self):
        buffer = bytearray()
        dropping = False
        try:
            while True:
                chunk = os.read(self.read_fd, 65536)
                if not chunk:
                    break
                for fragment in chunk.split(b'\0')[:-1]:
                    if not dropping:
                        buffer.extend(fragment)
                        if len(buffer) <= MAX_FRAME:
                            try:
                                value = json.loads(buffer)
                            except (ValueError, UnicodeError):
                                value = None
                            if isinstance(value, dict):
                                with self.condition:
                                    if type(value.get('id')) is int and value['id'] == self.pending:
                                        self.reply = value
                                        self.condition.notify_all()
                    buffer.clear()
                    dropping = False
                tail = chunk.rsplit(b'\0', 1)[-1]
                if not dropping:
                    buffer.extend(tail)
                    if len(buffer) > MAX_FRAME:
                        buffer.clear()
                        dropping = True
        except OSError:
            pass
        finally:
            with self.condition:
                self.failure = True
                self.condition.notify_all()

    def call(self, method, params=None, session=None, deadline=None):
        deadline = deadline if deadline is not None else time.monotonic() + TIMEOUT
        with self.commands:
            with self.condition:
                self.sequence += 1
                request_id = self.sequence
                self.pending, self.reply = request_id, None
            value = {'id': request_id, 'method': method, 'params': params or {}}
            if session is not None:
                value['sessionId'] = session
            data = json.dumps(value, separators=(',', ':'), allow_nan=False).encode() + b'\0'
            if len(data) > MAX_REQUEST:
                raise ValueError('Native debugger command too large')
            try:
                with selectors.DefaultSelector() as selector:
                    selector.register(self.write_fd, selectors.EVENT_WRITE)
                    while data:
                        remaining = deadline - time.monotonic()
                        if remaining <= 0 or not selector.select(remaining):
                            raise ValueError('Native debugger command timed out')
                        try:
                            count = os.write(self.write_fd, data)
                            data = data[count:]
                        except BlockingIOError:
                            continue
                with self.condition:
                    while self.reply is None and not self.failure:
                        remaining = deadline - time.monotonic()
                        if remaining <= 0:
                            raise ValueError('Native debugger command timed out')
                        self.condition.wait(remaining)
                    value = self.reply
                if not isinstance(value, dict) or value.get('error') or not isinstance(value.get('result'), dict):
                    raise ValueError('Native debugger command failed')
                return value['result']
            except OSError:
                raise ValueError('Native debugger pipe unavailable') from None
            finally:
                with self.condition:
                    self.pending, self.reply = None, None

    def evaluate(self, expression):
        if not isinstance(expression, str) or not expression:
            raise ValueError('Native debugger expression invalid')
        deadline = time.monotonic() + TIMEOUT
        targets = self.call('Target.getTargets', deadline=deadline).get('targetInfos')
        if not isinstance(targets, list):
            raise ValueError('Native renderer unavailable')
        matches = [target for target in targets if isinstance(target, dict)
                   and target.get('type') == 'page' and target.get('url') == 'app://-/index.html'
                   and isinstance(target.get('targetId'), str) and target['targetId']]
        if not matches:
            raise ValueError('Native renderer unavailable')
        target = min(matches, key=lambda item: item['targetId'])
        session = self.call('Target.attachToTarget', {'targetId': target['targetId'], 'flatten': True},
                            deadline=deadline).get('sessionId')
        if not isinstance(session, str) or not session:
            raise ValueError('Native renderer session unavailable')
        try:
            result = self.call('Runtime.evaluate', {'expression': expression, 'awaitPromise': True,
                                                   'returnByValue': True}, session, deadline)
            if result.get('exceptionDetails') or not isinstance(result.get('result'), dict):
                raise ValueError('Native API invocation failed')
            return result['result'].get('value')
        finally:
            try:
                self.call('Target.detachFromTarget', {'sessionId': session},
                          deadline=min(deadline, time.monotonic() + 2))
            except ValueError:
                pass


def spawn_app(executable, arguments, environment):
    """Spawn without a shell or HTTP debugging listener; return pid/read/write."""
    child_read, parent_write = os.pipe()
    parent_read, child_write = os.pipe()
    descriptors = [child_read, parent_write, parent_read, child_write]
    # Avoid a source descriptor being overwritten when dup2 maps the fixed fds.
    import fcntl
    try:
        for index, descriptor in enumerate(descriptors):
            if descriptor < 5:
                replacement = fcntl.fcntl(descriptor, fcntl.F_DUPFD_CLOEXEC, 5)
                os.close(descriptor)
                descriptors[index] = replacement
        child_read, parent_write, parent_read, child_write = descriptors
        actions = [(os.POSIX_SPAWN_DUP2, child_read, 3),
                   (os.POSIX_SPAWN_DUP2, child_write, 4)]
        actions += [(os.POSIX_SPAWN_CLOSE, descriptor) for descriptor in descriptors]
        pid = os.posix_spawn(str(executable), [str(executable), *arguments, '--remote-debugging-pipe'],
                             environment, file_actions=actions,
                             setsigdef=(signal.SIGINT, signal.SIGTERM))
    except BaseException:
        for descriptor in descriptors:
            os.close(descriptor)
        raise
    os.close(child_read)
    os.close(child_write)
    return pid, parent_read, parent_write


class Broker:
    def __init__(self, root, app_pid, evaluator):
        self.root = _root(root)
        self.app_pid = app_pid
        self.evaluator = evaluator
        self.path = self.root / SOCKET_NAME
        if self.path.exists() or self.path.is_symlink():
            raise ValueError('Native debugger socket already exists')
        self.listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.inode = None
        try:
            self.listener.bind(str(self.path))
            self.inode = self.path.lstat().st_ino
            os.chmod(self.path, 0o600)
            self.listener.listen(4)
            self.listener.settimeout(0.25)
        except BaseException:
            self.close()
            raise

    def once(self):
        try:
            connection, _ = self.listener.accept()
        except TimeoutError:
            return
        with connection:
            connection.settimeout(TIMEOUT + 5)
            try:
                if _peer_uid(connection) != os.getuid():
                    raise ValueError('Native debugger peer mismatch')
                request = _receive(connection, MAX_REQUEST)
                if not isinstance(request, dict):
                    raise ValueError('Native debugger request invalid')
                if request == {'operation': 'identity'}:
                    response = {'app_pid': self.app_pid, 'broker_pid': os.getpid()}
                elif (set(request) == {'operation', 'app_pid', 'expression'}
                      and request['operation'] == 'evaluate'
                      and type(request['app_pid']) is int and request['app_pid'] == self.app_pid
                      and isinstance(request['expression'], str) and request['expression']):
                    response = {'app_pid': self.app_pid, 'value': self.evaluator(request['expression'])}
                else:
                    raise ValueError('Native debugger request invalid')
                _send(connection, {'ok': True, **response}, MAX_FRAME)
            except (OSError, ValueError, UnicodeError):
                try:
                    _send(connection, {'ok': False, 'error': 'Native debugger request failed'}, MAX_FRAME)
                except (OSError, ValueError):
                    pass

    def close(self):
        self.listener.close()
        try:
            if self.inode is not None and self.path.lstat().st_ino == self.inode:
                self.path.unlink()
        except FileNotFoundError:
            pass


def _request(value, root=None):
    root = _root(root)
    path = root / SOCKET_NAME
    info = path.lstat()
    if (not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid()
            or stat.S_IMODE(info.st_mode) != 0o600):
        raise ValueError('Native debugger socket must be owned and mode 0600')
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
        connection.settimeout(TIMEOUT + 5)
        connection.connect(str(path))
        if _peer_uid(connection) != os.getuid():
            raise ValueError('Native debugger peer mismatch')
        _send(connection, value, MAX_REQUEST)
        response = _receive(connection, MAX_FRAME)
    if not isinstance(response, dict) or response.get('ok') is not True:
        raise ValueError('Native debugger request failed')
    return response


def identity(root=None):
    response = _request({'operation': 'identity'}, root)
    if any(type(response.get(name)) is not int or response[name] <= 0 for name in ('app_pid', 'broker_pid')):
        raise ValueError('Native debugger identity invalid')
    return {name: response[name] for name in ('app_pid', 'broker_pid')}


def evaluate(expression, expected_pid, root=None):
    if type(expected_pid) is not int or expected_pid <= 0:
        raise ValueError('Native debugger app identity invalid')
    response = _request({'operation': 'evaluate', 'app_pid': expected_pid, 'expression': expression}, root)
    if response.get('app_pid') != expected_pid:
        raise ValueError('Native debugger app identity changed')
    return response.get('value')


def _state(root, app_pid):
    path = root / STATE_NAME
    temporary = root / (STATE_NAME + '.tmp')
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(fd, 'w') as stream:
            json.dump({'app_pid': app_pid, 'broker_pid': os.getpid()}, stream)
            stream.flush()
            os.fsync(stream.fileno())
        temporary.replace(path)
        return path.lstat().st_ino
    finally:
        temporary.unlink(missing_ok=True)


def serve(executable, arguments, environment):
    """Run the broker until its exact child app exits; never terminate that app."""
    root = _root()
    expected = settings.APP / 'Contents/MacOS/ChatGPT'
    if pathlib.Path(executable) != expected:
        raise ValueError('Native debugger requires the configured official executable')
    if any(argument.startswith('--remote-debugging-') for argument in arguments):
        raise ValueError('Native debugger flags are broker-owned')
    stop = threading.Event()
    previous = {number: signal.signal(number, lambda *_: stop.set())
                for number in (signal.SIGINT, signal.SIGTERM)}
    pid, read_fd, write_fd = None, None, None
    broker = None
    state_inode = None
    reaped = False
    try:
        # Reserve the private endpoint before starting the app. Existing sockets
        # fail closed, including stale sockets; they are never silently replaced.
        broker = Broker(root, 0, None)
        pid, read_fd, write_fd = spawn_app(executable, arguments, environment)
        pipe = Pipe(read_fd, write_fd)
        broker.app_pid, broker.evaluator = pid, pipe.evaluate
        state_inode = _state(root, pid)
        while not stop.is_set():
            if os.waitpid(pid, os.WNOHANG)[0] == pid:
                reaped = True
                break
            broker.once()
    finally:
        if broker is not None:
            try:
                broker.close()
            except OSError:
                # Cleanup failure must not close the app's debugging pipe.
                pass
        # Closing either pipe while Electron is alive asks Electron to quit.
        # Even an IPC failure must therefore wait for the user's own app exit.
        if pid is not None and not reaped:
            while True:
                try:
                    os.waitpid(pid, 0)
                    break
                except InterruptedError:
                    continue
        for descriptor in (read_fd, write_fd):
            if descriptor is not None:
                os.close(descriptor)
        if state_inode is not None:
            path = root / STATE_NAME
            try:
                if path.lstat().st_ino == state_inode:
                    path.unlink()
            except FileNotFoundError:
                pass
        for number, handler in previous.items():
            signal.signal(number, handler)


if __name__ == '__main__':
    serve(settings.APP / 'Contents/MacOS/ChatGPT', sys.argv[1:], os.environ.copy())
