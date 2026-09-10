#!/usr/bin/env python3
"""Render/check specification documents. Never execute CQ or access its state."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import shlex
import sys
import textwrap

BASE = Path(__file__).resolve().parent
SPEC = json.loads((BASE / 'commands.json').read_text())
COMMANDS = SPEC['commands']
GLOBALS = SPEC['global_options']
BY_PATH = {c['path']: c for c in COMMANDS}

def usage(c):
    parts = ['cq', c['path']]
    if c['kind'] == 'group':
        parts.append('<COMMAND>')
    else:
        for p in c['positionals']:
            value = p['metavar'] or p['name'].upper().replace('-', '_')
            if p['repeatable']: value += '...'
            parts.append(value if p['required'] else '[' + value + ']')
        for p in c['options']:
            if p['required']:
                parts.append('--' + p['name'] + ('' if p['type'] == 'boolean' else ' ' + p['metavar']))
    parts.append('[OPTIONS]')
    return ' '.join(parts)

def param_label(p, positional=False):
    if positional: return p['metavar'] or p['name'].upper()
    label = ('-' + p['short'] + ', ' if p.get('short') else '') + '--' + p['name']
    return label if p['type'] == 'boolean' else label + ' ' + (p['metavar'] or 'VALUE')

def param_text(p):
    text = p['help'] + '\n'
    text += 'Type: ' + p['type'] + '. Required: ' + str(p['required']).lower() + '. Repeatable: ' + str(p['repeatable']).lower() + '.\n'
    text += 'Default: ' + json.dumps(p['default'], ensure_ascii=False) + '.\n'
    if p.get('choices'): text += 'Choices: ' + ', '.join(p['choices']) + '.\n'
    if p.get('omission_resolution'): text += 'When omitted: ' + p['omission_resolution'] + '\n'
    for rule in p['validation']: text += 'Constraint: ' + rule + '\n'
    return text.rstrip()

def help_text(c):
    lines=[c['summary'], '', 'Usage: '+usage(c), '']
    for paragraph in c['description']:
        lines += textwrap.wrap(paragraph, width=88)+['']
    if c['kind']=='group':
        children=[x for x in COMMANDS if x['path'].rsplit(' ',1)[0]==c['path']]
        lines += ['Commands:']
        for child in children:
            lines += ['  '+child['path'].split()[-1], '      '+child['summary']]
        lines += ['']
    for heading, params, positional in [('Arguments:',c['positionals'],True),('Options:',c['options'],False),('Global options:',GLOBALS,False)]:
        if not params:continue
        lines.append(heading)
        for p in params:
            lines.append('  '+param_label(p,positional))
            for paragraph in param_text(p).splitlines():
                lines += textwrap.wrap(paragraph,width=88,initial_indent='      ',subsequent_indent='      ')
        lines.append('')
    lines.append('Examples:')
    for example in c['examples']:
        lines+=['  '+example['command'],'      '+example['description']]
    return '\n'.join(lines).rstrip()+'\n'

def root_help():
    lines=['Check provider quotas and manage accounts, routing and services.','','Usage: cq [COMMAND] [OPTIONS]','','Without a command, check Claude, Codex and Gemini quotas.','Use cq check codex to check Codex only.','','Commands:']
    for c in COMMANDS:
        if ' ' not in c['path']:lines+=['  '+c['path'],'      '+c['summary']]
    lines+=['','Global options:']
    for p in GLOBALS:
        lines+=['  '+param_label(p)]
        for paragraph in param_text(p).splitlines():
            lines+=textwrap.wrap(paragraph,width=88,initial_indent='      ',subsequent_indent='      ')
    lines+=['','Examples:','  cq','  cq check codex --fresh','  cq codex proxy --help','  cq help service install']
    return '\n'.join(lines)+'\n'

def render():
    files={'help/cq.txt':root_help()}
    doc=['# Canonical command reference','','Generated from [commands.json](commands.json). Do not edit generated files.','', 'Every option below inherits the parser, output, consent and error rules in [README.md](README.md). Exact help includes global options at every node. Null defaults mean no literal default; command rules define omission.','']
    acceptance=['# Acceptance catalogue','','These are implementation requirements, not claims that the current binary passes. Commands must also pass the global cases in README.md.','']
    for c in COMMANDS:
        slug=c['path'].replace(' ','-')
        files['help/'+slug+'.txt']=help_text(c)
        doc+=['## `cq '+c['path']+'`','',c['summary'],'','```text',usage(c),'```','','[Exact help](help/'+slug+'.txt) · Family: '+c['family']+' · Kind: '+c['kind'],'']
        if c.get('availability',{}).get('supported') is False:
            doc+=['**Availability:** unavailable. Well-formed execution returns exit 4, `data=null`, and no human stdout. Reserved success semantics in the JSON catalogue are design notes, not enabled behaviour.','']
        doc+=c['description']+['']
        doc+=['### Naming rationale','']+['- `'+k+'`: '+v for k,v in c['terms'].items()]+['']
        for heading,params,pos in [('Arguments',c['positionals'],True),('Options',c['options'],False)]:
            doc+=['### '+heading,'']
            if not params:doc+=['None.',''];continue
            for p in params:
                doc+=['#### `'+param_label(p,pos)+'`','',param_text(p).replace('\n','\n\n'),'']
        for heading,key in [('Preconditions','preconditions'),('Effects and completion','effects')]:
            doc+=['### '+heading,'']+(['- '+v for v in c[key]] or ['None beyond global rules.'])+['']
        doc+=['### Output','','Fields below belong to envelope `data`; global envelope keys are specified once in README.md.','']
        doc+=['- `'+k+'`: '+str(v) for k,v in c['output']['fields'].items()]
        doc+=['','Human output template:','','```text',c['output']['human'],'```','']
        doc+=['### Command errors','','| Code | Exit | Condition | Exact message |','| --- | ---: | --- | --- |']
        esc=lambda x:str(x).replace('|','\\|').replace('\n','<br>')
        doc += ['| '+' | '.join(esc(e[k]) for k in ['code','exit','condition','message'])+' |' for e in c['errors']]
        doc+=['','### Examples','']
        for e in c['examples']:doc+=['```sh',e['command'],'```','',e['description'],'']
        doc+=['### Compatibility spellings','']
        for a in c['aliases']:doc+=['- `cq '+a['path']+'` → `'+a['translation']+'`. '+a['note']]
        if not c['aliases']:doc+=['No additional spellings; see migration inventory for old flags on unchanged paths.']
        doc+=['','### Source evidence','']+['- `'+s+'`' for s in c['sources']]+['']
        acceptance+=['## `cq '+c['path']+'`','']+['- [ ] '+s for s in c['tests']]+['']
    files['COMMANDS.md']='\n'.join(doc).rstrip()+'\n'
    files['acceptance.md']='\n'.join(acceptance).rstrip()+'\n'
    return files

def validate():
    errors=[]
    required=['id','path','summary','description','terms','positionals','options','preconditions','effects','output','errors','examples','aliases','tests','sources','kind']
    if len(BY_PATH)!=len(COMMANDS):errors.append('Duplicate command paths')
    all_examples=0
    for c in COMMANDS:
        path=c['path']
        for key in required:
            if key not in c:errors.append(path+': missing '+key)
        for word in path.split():
            if not c['terms'].get(word):errors.append(path+': missing term '+word)
        if not c['examples'] or not c['tests']:errors.append(path+': missing examples/tests')
        for seq in [c['positionals'],c['options']]:
            names=[p['name'] for p in seq]
            if len(names)!=len(set(names)):errors.append(path+': duplicate parameters')
            for p in seq:
                for k in ['name','type','required','repeatable','default','metavar','help','validation']:
                    if k not in p:errors.append(path+': parameter missing '+k)
                if not p['help']:errors.append(path+': empty parameter help')
                if p['type']=='enum' and not p.get('choices'):errors.append(path+': enum without choices')
                if p['default'] is not None and not p['repeatable']:
                    if p['type']=='integer' and type(p['default']) is not int:errors.append(path+': non-integer default '+p['name'])
                    if p['type']=='boolean' and type(p['default']) is not bool:errors.append(path+': non-boolean default '+p['name'])
        allowed={ '--'+p['name']:p for p in c['options']+GLOBALS }
        allowed.update({'-'+p['short']:p for p in c['options']+GLOBALS if p.get('short')})
        for example in c['examples']:
            all_examples+=1
            try:tokens=shlex.split(example['command'])
            except ValueError as e:errors.append(path+': invalid shell example '+str(e));continue
            if '|' in tokens and tokens[tokens.index('|')+1:][:1]==['cq']: tokens=tokens[tokens.index('|')+1:]
            if not tokens or tokens[0]!='cq':errors.append(path+': example must invoke cq');continue
            # Examples may intentionally demonstrate a sibling command; validate its own declared options.
            resolved=None
            for candidate in sorted(COMMANDS,key=lambda x:len(x['path']),reverse=True):
                words=candidate['path'].split()
                if tokens[1:1+len(words)]==words:resolved=candidate;break
            if resolved:
                legal={'--'+p['name'] for p in resolved['options']+GLOBALS}
                legal|={'-'+p['short'] for p in resolved['options']+GLOBALS if p.get('short')}
                for token in tokens:
                    if token.startswith('--') and token.split('=',1)[0] not in legal:errors.append(path+': unknown example option '+token)
                rest=tokens[1+len(resolved['path'].split()):]
                # Redirection is performed by the example shell, not passed to CQ.
                if '>' in rest:rest=rest[:rest.index('>')]
                parameters={'--'+p['name']:p for p in resolved['options']+GLOBALS}
                used=set();positionals=[];i=0
                while i<len(rest):
                    token=rest[i];key=token.split('=',1)[0]
                    if key in parameters:
                        p=parameters[key];used.add(p['name'])
                        if p['type']!='boolean':
                            if '=' in token:value=token.split('=',1)[1]
                            else:
                                i+=1
                                if i>=len(rest):errors.append(path+': missing example value '+key);break
                                value=rest[i]
                            if p.get('choices') and '$' not in value and value not in p['choices']:
                                errors.append(path+': invalid example choice '+value)
                    else:positionals.append(token)
                    i+=1
                if not used.intersection({'help','version'}) and resolved['kind']!='group':
                    for p in resolved['options']:
                        if p['required'] and p['name'] not in used:errors.append(path+': missing required example option '+p['name'])
                    minimum=sum(p['required'] for p in resolved['positionals'])
                    maximum=None if any(p['repeatable'] for p in resolved['positionals']) else len(resolved['positionals'])
                    if len(positionals)<minimum or (maximum is not None and len(positionals)>maximum):
                        errors.append(path+': incorrect example positional count')
    # Legacy inventory is a coverage ledger, not a second command definition.
    migration=json.loads((BASE/'migration.json').read_text())
    inventory=json.loads((BASE/'source-inventory.json').read_text())
    source_paths={row['path'] for row in inventory}
    migration_paths=[row['legacy_path'] for row in migration]
    if len(migration_paths)!=len(set(migration_paths)) or set(migration_paths)!=source_paths:
        errors.append('Migration coverage differs from source inventory')
    for row in migration:
        dest=row.get('canonical_path')
        if dest and dest not in BY_PATH:errors.append('Unknown migration destination: '+dest)
        for variant in row.get('variants',[]):
            dest=variant.get('canonical_path')
            if dest and dest not in BY_PATH:errors.append('Unknown variant destination: '+dest)
        if row.get('abi_contract') and not (BASE/row['abi_contract'].split('#')[0]).exists():
            errors.append('Missing ABI contract '+row['abi_contract'])
    for annex in BASE.glob('*-annex-v1'):
        manifest=json.loads((annex/'manifest.json').read_text())
        for record in manifest['files']:
            path=annex/record['path']
            if not path.exists() or hashlib.sha256(path.read_bytes()).hexdigest()!=record['sha256']:
                errors.append('Annex hash mismatch: '+str(path))
        if list(annex.rglob('*.go')):errors.append('Annex contains buildable Go sources: '+str(annex))
    for md in BASE.rglob('*.md'):
        content=md.read_text()
        for target in re.findall(r'\]\(([^)]+)\)',content):
            if target.startswith(('http:', 'https:', '#')):continue
            target=target.split('#')[0]
            if not (md.parent/target).exists():errors.append(str(md.relative_to(BASE))+': broken link '+target)
    return errors,all_examples

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--check',action='store_true',help='Validate catalogue and compare generated files without writing.')
    args=parser.parse_args()
    errors,examples=validate()
    for relative,content in render().items():
        dest=BASE/relative
        if args.check:
            if not dest.exists() or dest.read_text()!=content:errors.append(relative+': generated content differs')
        else:
            dest.parent.mkdir(parents=True,exist_ok=True);dest.write_text(content)
    if errors:
        print('\n'.join(errors),file=sys.stderr);return 1
    print(f"Validated {len(COMMANDS)} command/group entries, {examples} examples and {len(COMMANDS)+1} exact help pages.")
    return 0
if __name__=='__main__':sys.exit(main())
