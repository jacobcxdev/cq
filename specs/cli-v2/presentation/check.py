#!/usr/bin/env python3
"""Check draft ownership and explicit command/error coverage, not design quality."""
import json,pathlib,re
root=pathlib.Path(__file__).resolve().parent
catalogue=json.loads((root.parent/'commands.json').read_text())['commands']
coverage=json.loads((root/'coverage.json').read_text())
assert set(coverage['commands'])=={c['path'] for c in catalogue if c['kind']=='command'}
assert set(coverage['groups'])=={c['path'] for c in catalogue if c['kind']=='group'}
errors=[]
assert '## `cq help`' in (root/coverage['root_help']).read_text()
for command in catalogue:
 path=command['path'];owner=coverage['groups'][path] if command['kind']=='group' else coverage['commands'][path]['document'];text=(root/owner).read_text()
 chunks=re.split(r'(?=^## )',text,flags=re.M)
 matching=[s for s in chunks if re.match(r'^## (?:`)?cq '+re.escape(path)+r'(?:`)?\s*$',s.splitlines()[0])]
 if len(matching)!=1:errors.append(f'{path}: expected one exact heading in {owner}');continue
 section=matching[0]
 if command['kind']=='command':
  for option in command['options']:
   if '--'+option['name'] not in section:errors.append(f'{path}: missing option --{option["name"]}')
  for e in command['errors']:
   if e['code'] not in section:errors.append(f'{path}: missing error {e["code"]}')
  if not re.search(r'help',section,re.I):errors.append(f'{path}: missing help design')
if errors:raise SystemExit('\n'.join(errors))
print(f'Covered {len(coverage["commands"])} leaves, {len(coverage["groups"])} groups, root help and every local option and declared per-command error code.')
print('This checks presence/ownership only; semantic and visual review remain required.')
