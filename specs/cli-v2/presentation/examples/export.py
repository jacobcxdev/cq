#!/usr/bin/env python3
"""Export authored output blocks verbatim; never render command metadata."""
import argparse
import hashlib
import json
import pathlib
import re

ROOT = pathlib.Path(__file__).resolve().parent
SOURCES = ('groups.md', 'accounts.md', 'routing.md', 'runtime.md', 'dots.md')


def collect():
    records = []
    sections = {}
    for source in SOURCES:
        lines = (ROOT / source).read_text().splitlines(keepends=True)
        command = None
        scenario = ''
        subheading = ''
        context = []
        counts = {}
        scenario_start = 0
        index = 0
        while index < len(lines):
            line = lines[index].rstrip('\n')
            match = re.fullmatch(r'## `(.+)`', line)
            if match:
                command = match[1]
                if command in sections:
                    raise ValueError(f'Duplicate command: {command}')
                sections[command] = {'source': source, 'line': index + 1, 'body': ''}
                scenario = subheading = ''
                scenario_start = index
                context = []
            elif line.startswith('### '):
                scenario = line[4:]
                scenario_start = index
                subheading = ''
                context = []
            elif line.startswith('#### '):
                subheading = line[5:]
                context = []
            if command:
                sections[command]['body'] += lines[index]
            if line in ('```text', '```json', '```jsonl') and command:
                start = index + 2
                body = []
                index += 1
                while index < len(lines) and lines[index].strip() != '```':
                    body.append(lines[index])
                    sections[command]['body'] += lines[index]
                    index += 1
                if index == len(lines):
                    raise ValueError(f'Unclosed fence: {source}:{start}')
                output = ''.join(body)
                if not output:
                    raise ValueError(f'Empty fence: {source}:{start}; describe empty stream instead')
                ordinal = counts.get(command, 0) + 1
                counts[command] = ordinal
                slug = 'cq-root-help' if command == 'cq --help' else re.sub(r'[^a-z0-9]+', '-', command.lower()).strip('-')
                case = re.sub(r'[^a-z0-9]+', '-', scenario.lower()).strip('-')
                filename = f'text/{slug}/{ordinal:02d}-{case or "output"}.txt'
                stream = 'unspecified'
                for entry in reversed(context):
                    found = re.findall(r'\b(stdout|stderr)\b', entry, re.I)
                    if found:
                        stream = found[-1].lower()
                        break
                end = index + 1
                while end < len(lines) and not re.match(r'^#{2,3} ', lines[end]):
                    end += 1
                scenario_text = ''.join(lines[scenario_start:end])
                invocation_match = re.search(r'Invocation:\s*(?:`([^`]+)`|```sh\n([\s\S]*?)\n```)', scenario_text)
                invocation = None
                if invocation_match:
                    invocation = invocation_match[1] or invocation_match[2]
                else:
                    shell_match = re.search(r'```sh\n([\s\S]*?)\n```', scenario_text)
                    if shell_match:
                        invocation = shell_match[1]
                metadata = re.sub(r'```[^\n]*\n[\s\S]*?```', '', scenario_text)
                exit_match = re.search(r'\bexit\s*:?\s*`?(\d+|pending)\b', metadata, re.I)
                exit_status = None if not exit_match else exit_match[1]
                if exit_status and exit_status.isdigit():
                    exit_status = int(exit_status)
                records.append({
                    'command': command,
                    'scenario': scenario,
                    'variant': subheading,
                    'stream': stream,
                    'invocation': invocation,
                    'exit': exit_status,
                    'source': source,
                    'line': start,
                    'file': filename,
                    'sha256': hashlib.sha256(output.encode()).hexdigest(),
                    'context': ''.join(context).strip(),
                    '_output': output,
                })
                context = []
            else:
                context.append(lines[index])
            index += 1
    account_lines = (ROOT / 'accounts.md').read_text().splitlines()
    for shell in ('bash', 'zsh', 'fish'):
        path = ROOT / 'raw' / ('completion.' + shell)
        output = path.read_text()
        source_line = next(i + 1 for i, line in enumerate(account_lines) if line.startswith('Invocation: `cq completion ' + shell + '`'))
        records.append({
            'command': 'cq completion', 'scenario': 'Success — ' + shell + ' raw protocol',
            'variant': '', 'stream': 'stdout', 'invocation': 'cq completion ' + shell,
            'exit': 0, 'source': 'accounts.md', 'line': source_line,
            'file': 'raw/completion.' + shell,
            'sha256': hashlib.sha256(output.encode()).hexdigest(),
            'context': 'Frozen raw protocol; copied exactly from the immutable completion catalogue.',
            'frozen_protocol': True, '_output': output,
        })
    return sections, records


def validate(sections, records):
    catalogue = json.loads((ROOT.parent.parent / 'commands.json').read_text())['commands']
    expected = {'cq ' + entry['path'] for entry in catalogue} | {'cq --help'}
    if set(sections) != expected:
        raise ValueError(f'Section mismatch: missing={sorted(expected-set(sections))}; extra={sorted(set(sections)-expected)}')
    for entry in catalogue:
        command = 'cq ' + entry['path']
        body = sections[command]['body']
        help_blocks = [r for r in records if r['command'] == command and 'help' in r['scenario'].lower()]
        if not help_blocks:
            raise ValueError(f'No help output: {command}')
        help_text = '\n'.join(r['_output'] for r in help_blocks)
        for option in entry['options']:
            if '--' + option['name'] not in help_text:
                raise ValueError(f'Missing help option: {command} --{option["name"]}')
        for global_option in ('--help', '--json', '--version'):
            if global_option not in help_text:
                raise ValueError(f'Missing global help option: {command} {global_option}')
        unavailable = {
            'proxy candidate client-safety refresh', 'proxy candidate start',
            'proxy candidate release activate', 'proxy candidate release validate',
        }
        if entry['kind'] == 'command' and entry['path'] not in unavailable:
            if not re.search(r'^### Success', body, re.M):
                raise ValueError(f'No success scenario: {command}')
        if not re.search(r'^### .*fail', body, re.M | re.I):
            raise ValueError(f'No failure scenario: {command}')
    if not any(r['command'] == 'cq --help' and 'help' in r['scenario'].lower() for r in records):
        raise ValueError('Missing root help output')
    return catalogue


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--check', action='store_true')
    args = parser.parse_args()
    sections, records = collect()
    catalogue = validate(sections, records)
    outputs = {r['file']: r['_output'] for r in records}
    if len(outputs) != len(records):
        raise ValueError('Fixture filenames are not unique')
    missing_metadata = [r['file'] for r in records if not r['invocation'] or r['exit'] is None]
    if missing_metadata:
        raise ValueError(f'Missing invocation/exit metadata: {missing_metadata}')
    unspecified = [r['file'] for r in records if r['stream'] == 'unspecified']
    if unspecified:
        raise ValueError(f'Missing stream labels: {unspecified}')
    clean = [{key: value for key, value in r.items() if key != '_output'} for r in records]
    outputs['index.json'] = json.dumps({
        'status': 'hand-authored design examples; not observed runtime output',
        'clock': '2026-09-23T15:30:00Z',
        'timezone': 'Europe/London',
        'terminal_columns': 100,
        'commands': len([c for c in catalogue if c['kind'] == 'command']),
        'help_pages': len(sections),
        'fixtures': clean,
    }, indent=2, ensure_ascii=False) + '\n'
    differences = []
    for name, content in outputs.items():
        path = ROOT / name
        if args.check:
            if not path.exists() or path.read_text() != content:
                differences.append(name)
        else:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content)
    extras = {str(p.relative_to(ROOT)) for p in (ROOT / 'text').rglob('*.txt')} - set(outputs)
    if extras:
        raise ValueError(f'Unexpected stale exports; review before removing: {sorted(extras)}')
    if differences:
        raise ValueError(f'Exports differ: {differences}')
    print(f'{len(sections)} command/help pages; {len(records)} verbatim output fixtures; all local and global help options covered.')


if __name__ == '__main__':
    main()
