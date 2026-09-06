#!/usr/bin/env python3
"""Run beside a Linux dune binary in an exclusively owned temporary directory.
Usage: python3 remote_agent.py /absolute/path/to/codex
The caller is responsible for deleting the temporary directory after collection.
"""
import json
from pathlib import Path
import socket
import subprocess
import sys
import time

root = Path(__file__).resolve().parent
binary = root / 'dune'
config = root / 'config.yaml'
work = root / 'task'
work.mkdir(exist_ok=True)
(work / 'check.py').write_text("from arithmetic import add\nassert add(2, 3) == 5\nassert add(-7, 4) == -3\nprint('DUNE_REAL_AGENT_CHECK_OK')\n")
with socket.socket() as sock:
    sock.bind(('127.0.0.1', 0))
    port = sock.getsockname()[1]
base = [str(binary), '--config', str(config)]
subprocess.run(base + ['init', f'127.0.0.1:{port}'], check=True)
log = (root / 'service.log').open('w')
supervisor = subprocess.Popen(base, stdout=log, stderr=log)
try:
    for attempt in range(100):
        result = subprocess.run(base + ['capabilities'], capture_output=True)
        if result.returncode == 0:
            break
        time.sleep(0.1)
    else:
        raise RuntimeError('fabricd did not register')
    prompt = ('In this directory create arithmetic.py defining add(a, b) returning their sum. '
              'Run python3 check.py and confirm it passes. Only modify arithmetic.py; '
              'do not access other directories.')
    argv = [sys.argv[1], 'exec', '--ephemeral', '--skip-git-repo-check',
            '--sandbox', 'workspace-write', prompt]
    profile = {'version': 1, 'kind': 'agent', 'working_directory': str(work), 'adapter': 'pty',
               'start': {'argv': argv, 'timeout_seconds': 150}}
    profile_path = root / 'profile.json'
    profile_path.write_text(json.dumps(profile))
    agent_log = root / 'agent.log'
    with agent_log.open('w') as out:
        agent = subprocess.run(base + ['profile', 'start', str(profile_path)], stdin=subprocess.DEVNULL,
                               stdout=out, stderr=subprocess.STDOUT, timeout=170)
    print('Agent exit:', agent.returncode)
    print(agent_log.read_text(errors='replace')[-12000:])
    check = subprocess.run(base + ['exec', '--cwd', str(work), '--', 'python3', 'check.py'],
                           capture_output=True, text=True, timeout=15)
    print('Independent Dune Exec verification:', check.stdout, check.stderr)
    if agent.returncode or check.returncode or 'DUNE_REAL_AGENT_CHECK_OK' not in check.stdout:
        raise RuntimeError('real Agent task failed')
    print('REMOTE_REAL_AGENT_PASSED')
finally:
    supervisor.terminate()
    try:
        supervisor.wait(timeout=10)
    except subprocess.TimeoutExpired:
        supervisor.kill()
        supervisor.wait()
    log.close()
