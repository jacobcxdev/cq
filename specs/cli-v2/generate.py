#!/usr/bin/env python3
"""Render/check specification documents. Never execute CQ or access its state."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import shlex
import subprocess
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

def go_quote(value):
    return json.dumps(value, ensure_ascii=False)

def go_string_slice(values):
    values = values or []
    return '[]string{' + ', '.join(go_quote(str(value).lower() if isinstance(value, bool) else str(value)) for value in values) + '}'

def parameter_go(p):
    default = p['default']
    if default is None:
        defaults = 'nil'
    elif isinstance(default, list):
        defaults = go_string_slice(default)
    else:
        defaults = go_string_slice([default])
    return '\n'.join([
        '\t\t\t{',
        '\t\t\t\tName: ' + go_quote(p['name']) + ',',
        '\t\t\t\tType: ' + go_quote(p['type']) + ',',
        '\t\t\t\tShort: ' + go_quote(p.get('short') or '') + ',',
        '\t\t\t\tMetavar: ' + go_quote(p.get('metavar') or '') + ',',
        '\t\t\t\tChoices: ' + go_string_slice(p.get('choices')) + ',',
        '\t\t\t\tDefault: ' + defaults + ',',
        '\t\t\t\tRequired: ' + str(p['required']).lower() + ',',
        '\t\t\t\tRepeatable: ' + str(p['repeatable']).lower() + ',',
        '\t\t\t},',
    ])

def command_go(c):
    lines = [
        '\t{',
        '\t\tPath: ' + go_quote(c['path']) + ',',
        '\t\tKind: ' + go_quote(c['kind']) + ',',
        '\t\tOptions: []ParameterSpec{',
    ]
    lines.extend(parameter_go(p) for p in c['options'])
    lines += ['\t\t},', '\t\tPositionals: []ParameterSpec{']
    lines.extend(parameter_go(p) for p in c['positionals'])
    lines += ['\t\t},', '\t},']
    return '\n'.join(lines)

def shell_lines(values):
    return ' '.join(shlex.quote(value) for value in values)

def children_by_path():
    paths = [''] + [c['path'] for c in COMMANDS]
    children = {}
    for path in paths:
        prefix = path.split()
        children[path] = sorted({
            c['path'].split()[len(prefix)]
            for c in COMMANDS
            if c['path'].split()[:len(prefix)] == prefix and len(c['path'].split()) == len(prefix) + 1
        })
    return children

def shell_case_function(name, rows):
    lines = [name + '() {', '  case "$1" in']
    for key, values in rows:
        lines += ['    ' + shlex.quote(key) + ') printf \'%s\\n\' ' + shell_lines(values) + ' ;;']
    lines += ['  esac', '}']
    return '\n'.join(lines)

def shell_metadata():
    children = children_by_path()
    kinds = [('', ['group'])] + [(c['path'], [c['kind']]) for c in COMMANDS]
    child_rows = [(path, words) for path, words in children.items() if words]
    option_rows = []
    enum_rows = []
    takes_rows = []
    repeatable_rows = []
    canonical_rows = []
    path_rows = []
    for p in GLOBALS:
        long = '--' + p['name']
        for spelling in [long] + (['-' + p['short']] if p.get('short') else []):
            canonical_rows.append(('|' + spelling, [long]))
    for c in COMMANDS:
        all_options = c['options'] + GLOBALS
        spellings = []
        for p in all_options:
            long = '--' + p['name']
            spellings.append(long)
            if p.get('short'):
                spellings.append('-' + p['short'])
            for spelling in [long] + (['-' + p['short']] if p.get('short') else []):
                canonical_rows.append((c['path'] + '|' + spelling, [long]))
                if p['type'] != 'boolean':
                    takes_rows.append((c['path'] + '|' + spelling, ['1']))
                if p['repeatable']:
                    repeatable_rows.append((c['path'] + '|' + spelling, ['1']))
                if p.get('choices'):
                    enum_rows.append((c['path'] + '|option|' + spelling, p['choices']))
                if p['type'] == 'path':
                    path_rows.append((c['path'] + '|option|' + spelling, ['1']))
        option_rows.append((c['path'], spellings))
        for index, p in enumerate(c['positionals']):
            if p.get('choices'):
                enum_rows.append((c['path'] + '|positional|' + str(index), p['choices']))
                if p['repeatable']:
                    enum_rows.append((c['path'] + '|positional|repeatable', p['choices']))
            if p['type'] == 'path':
                path_rows.append((c['path'] + '|positional|' + str(index), ['1']))
                if p['repeatable']:
                    path_rows.append((c['path'] + '|positional|repeatable', ['1']))
    root_options = []
    for p in GLOBALS:
        root_options.append('--' + p['name'])
        if p.get('short'):
            root_options.append('-' + p['short'])
    option_rows.insert(0, ('', root_options))
    return kinds, child_rows, option_rows, enum_rows, takes_rows, repeatable_rows, canonical_rows, path_rows

def bash_completion():
    kinds, children, options, enums, takes, repeatable, canonical, paths = shell_metadata()
    helpers = [
        shell_case_function('_cq_kind', kinds),
        shell_case_function('_cq_children', children),
        shell_case_function('_cq_options', options),
        shell_case_function('_cq_choices', enums),
        shell_case_function('_cq_takes_value', takes),
        shell_case_function('_cq_repeatable', repeatable),
        shell_case_function('_cq_canonical_option', canonical),
        shell_case_function('_cq_path_value', paths),
    ]
    body = r'''_cq_complete() {
  # Bash 4+ splits adjacent assignments at '='; Bash 3 retains one word.
  # Rejoin only option assignments before --, without changing word breaks or
  # consuming a whitespace-separated literal '=' positional/value.
  local -a words=()
  local i last stopped=0 line="${COMP_LINE:0:COMP_POINT}"
  for ((i=0; i<=COMP_CWORD; i++)); do
    last=$((${#words[@]}-1))
    if [[ "$stopped" -eq 0 && "${COMP_WORDS[i]}" == = && "$last" -ge 0 && "${words[last]}" == --?* && "$line" == *"${words[last]}="* ]]; then
      words[last]+="="
      if ((i<COMP_CWORD)); then ((i++)); words[last]+="${COMP_WORDS[i]}"; fi
    else
      words+=("${COMP_WORDS[i]}")
      [[ "${COMP_WORDS[i]}" == -- ]] && stopped=1
    fi
  done
  local cword=$((${#words[@]}-1))
  local cur="${words[cword]}" path="" word candidate expect="" canonical="" used=$'\n'
  local inline_option="" inline_prefix=""
  local after_options=0 positional=0
  for ((i=1; i<cword; i++)); do
    word="${words[i]}"
    if [[ -n "$expect" ]]; then expect=""; continue; fi
    if [[ "$word" == -- && "$after_options" -eq 0 ]]; then after_options=1; continue; fi
    if [[ "$(_cq_kind "$path")" != command ]]; then
      candidate="${path:+$path }$word"
      if [[ -n "$(_cq_kind "$candidate")" ]]; then path="$candidate"; continue; fi
    fi
    if [[ "$after_options" -eq 0 && "$word" == -* ]]; then
      canonical="$(_cq_canonical_option "$path|${word%%=*}")"
      [[ -n "$canonical" ]] && used+="$canonical"$'\n'
      if [[ "$word" != *=* && "$(_cq_takes_value "$path|${word%%=*}")" == 1 ]]; then expect="${word%%=*}"; fi
      continue
    fi
    ((positional++))
  done
  local candidates="" mode="words"
  if [[ "$after_options" -eq 0 && "$cur" == --*=* ]]; then
    inline_option="${cur%%=*}"
    if [[ "$(_cq_takes_value "$path|$inline_option")" == 1 ]]; then
      inline_prefix="$inline_option="
      # Readline retains the option prefix when '=' is a word break.
      [[ "$COMP_WORDBREAKS" == *=* ]] && inline_prefix=""
      cur="${cur#*=}"
      expect="$inline_option"
    fi
  fi
  if [[ -n "$expect" ]]; then
    candidates="$(_cq_choices "$path|option|$expect")"
    [[ "$(_cq_path_value "$path|option|$expect")" == 1 ]] && mode="files"
  elif [[ "$(_cq_kind "$path")" != command ]]; then
    candidates="$(_cq_children "$path")"
  else
    candidates="$(_cq_choices "$path|positional|$positional")"
    [[ "$(_cq_path_value "$path|positional|$positional")" == 1 ]] && mode="files"
    if [[ -z "$candidates" ]]; then candidates="$(_cq_choices "$path|positional|repeatable")"; fi
    if [[ "$(_cq_path_value "$path|positional|repeatable")" == 1 ]]; then mode="files"; fi
  fi
  if [[ "$after_options" -eq 0 && -z "$expect" ]]; then
    while IFS= read -r candidate; do
      [[ -z "$candidate" ]] && continue
      canonical="$(_cq_canonical_option "$path|$candidate")"
      if [[ "$(_cq_repeatable "$path|$candidate")" == 1 || "$used" != *$'\n'"$canonical"$'\n'* ]]; then
        candidates+="${candidates:+$'\n'}$candidate"
      fi
    done <<< "$(_cq_options "$path")"
  fi
  COMPREPLY=()
  if [[ "$mode" == files ]]; then
    while IFS= read -r candidate; do
      printf -v candidate '%q' "$inline_prefix$candidate"
      COMPREPLY+=("$candidate")
    done < <(compgen -f -- "$cur")
  else
    while IFS= read -r candidate; do
      COMPREPLY+=("$inline_prefix$candidate")
    done < <(compgen -W "$candidates" -- "$cur")
  fi
}
complete -F _cq_complete cq
'''
    return '# bash completion for cq; generated from specs/cli-v2/commands.json\n' + '\n\n'.join(helpers) + '\n\n' + body

def zsh_completion():
    kinds, children, options, enums, takes, repeatable, canonical, paths = shell_metadata()
    helpers = [
        shell_case_function('_cq_kind', kinds),
        shell_case_function('_cq_children', children),
        shell_case_function('_cq_options', options),
        shell_case_function('_cq_choices', enums),
        shell_case_function('_cq_takes_value', takes),
        shell_case_function('_cq_repeatable', repeatable),
        shell_case_function('_cq_canonical_option', canonical),
        shell_case_function('_cq_path_value', paths),
    ]
    body = r'''_cq() {
  local cur="${words[CURRENT]}" path="" word candidate expect="" canonical="" used=$'\n'
  local inline_option="" inline_prefix=""
  local after_options=0 positional=0 i candidates mode="words"
  for ((i=2; i<CURRENT; i++)); do
    word="${words[i]}"
    if [[ -n "$expect" ]]; then expect=""; continue; fi
    if [[ "$word" == -- && "$after_options" -eq 0 ]]; then after_options=1; continue; fi
    if [[ "$(_cq_kind "$path")" != command ]]; then
      candidate="${path:+$path }$word"
      if [[ -n "$(_cq_kind "$candidate")" ]]; then path="$candidate"; continue; fi
    fi
    if [[ "$after_options" -eq 0 && "$word" == -* ]]; then
      canonical="$(_cq_canonical_option "$path|${word%%=*}")"
      [[ -n "$canonical" ]] && used+="$canonical"$'\n'
      if [[ "$word" != *=* && "$(_cq_takes_value "$path|${word%%=*}")" == 1 ]]; then expect="${word%%=*}"; fi
      continue
    fi
    ((positional++))
  done
  if [[ "$after_options" -eq 0 && "$cur" == --*=* ]]; then
    inline_option="${cur%%=*}"
    if [[ "$(_cq_takes_value "$path|$inline_option")" == 1 ]]; then
      inline_prefix="$inline_option="
      cur="${cur#*=}"
      expect="$inline_option"
    fi
  fi
  if [[ -n "$expect" ]]; then
    candidates="$(_cq_choices "$path|option|$expect")"
    [[ "$(_cq_path_value "$path|option|$expect")" == 1 ]] && mode="files"
  elif [[ "$(_cq_kind "$path")" != command ]]; then
    candidates="$(_cq_children "$path")"
  else
    candidates="$(_cq_choices "$path|positional|$positional")"
    [[ "$(_cq_path_value "$path|positional|$positional")" == 1 ]] && mode="files"
    if [[ -z "$candidates" ]]; then candidates="$(_cq_choices "$path|positional|repeatable")"; fi
    if [[ "$(_cq_path_value "$path|positional|repeatable")" == 1 ]]; then mode="files"; fi
  fi
  if [[ "$after_options" -eq 0 && -z "$expect" ]]; then
    for candidate in "${(@f)$(_cq_options "$path")}"; do
      canonical="$(_cq_canonical_option "$path|$candidate")"
      if [[ "$(_cq_repeatable "$path|$candidate")" == 1 || "$used" != *$'\n'"$canonical"$'\n'* ]]; then
        if [[ -n "$candidates" ]]; then candidates+=$'\n'; fi
        candidates+="$candidate"
      fi
    done
  fi
  if [[ "$mode" == files ]]; then
    if [[ -n "$inline_prefix" ]]; then
      local -a path_candidates prefixed_paths
      path_candidates=("${cur}"*(N))
      for candidate in "${path_candidates[@]}"; do prefixed_paths+=("$inline_prefix$candidate"); done
      compadd -f -- "${prefixed_paths[@]}"
    else
      _files
    fi
  else
    local -a candidate_array
    candidate_array=("${(@f)candidates}")
    if [[ -n "$inline_prefix" ]]; then candidate_array=("${(@)candidate_array/#/$inline_prefix}"); fi
    compadd -Q -- "${candidate_array[@]}"
  fi
}
compdef _cq cq
'''
    return '#compdef cq\n# zsh completion for cq; generated from specs/cli-v2/commands.json\n' + '\n\n'.join(helpers) + '\n\n' + body

def fish_completion():
    children = children_by_path()
    lines = [
        '# fish completion for cq; generated from specs/cli-v2/commands.json',
        'complete -c cq -e',
        'function __cq_command_words',
        '    set -l tokens (commandline -opc)',
        '    set -e tokens[1]',
        '    set -l result',
        '    set -l options_open 1',
        '    for token in $tokens',
        '        if test $options_open -eq 1; and test "$token" = --',
        '            set options_open 0',
        '            continue',
        '        end',
        '        if test $options_open -eq 1',
        '            switch $token',
        "                case -h -j -v --help --json --version '-h=*' '-j=*' '-v=*' '--help=*' '--json=*' '--version=*'",
        '                    continue',
        '            end',
        '        end',
        '        set -a result $token',
        '    end',
        "    printf '%s\\n' $result",
        'end',
        'function __cq_path_is',
        '    set -l tokens (__cq_command_words)',
        '    set -l actual ""',
        '    set -l expected ""',
        '    if test (count $tokens) -gt 0',
        '        set actual (string join " " -- $tokens)',
        '    end',
        '    if test (count $argv) -gt 0',
        '        set expected (string join " " -- $argv)',
        '    end',
        '    test "$actual" = "$expected"',
        'end',
        'function __cq_has_path',
        '    set -l tokens (__cq_command_words)',
        '    test (count $tokens) -ge (count $argv); or return 1',
        '    for index in (seq (count $argv))',
        '        test "$tokens[$index]" = "$argv[$index]"; or return 1',
        '    end',
        'end',
        'function __cq_options_open',
        '    not contains -- -- (commandline -opc)',
        'end',
    ]
    for p in GLOBALS:
        condition = '__cq_path_is; and __cq_options_open; and not __fish_seen_argument -l ' + shlex.quote(p['name']) + ' -s ' + shlex.quote(p['short'])
        lines.append('complete -c cq -f -n ' + shlex.quote(condition) + ' -l ' + shlex.quote(p['name']) + ' -s ' + shlex.quote(p['short']))
    for path, words in children.items():
        condition = '__cq_path_is' + ((' ' + shell_lines(path.split())) if path else '')
        for word in words:
            lines.append('complete -c cq -f -n ' + shlex.quote(condition) + ' -a ' + shlex.quote(word))
    for c in COMMANDS:
        condition = '__cq_has_path ' + shell_lines(c['path'].split()) + '; and __cq_options_open'
        for p in c['options'] + GLOBALS:
            option_condition = condition
            if not p['repeatable']:
                seen = '__fish_seen_argument -l ' + shlex.quote(p['name'])
                if p.get('short'):
                    seen += ' -s ' + shlex.quote(p['short'])
                option_condition += '; and not ' + seen
            command = ['complete', '-c', 'cq', '-f', '-n', shlex.quote(option_condition), '-l', p['name']]
            if p.get('short'):
                command += ['-s', p['short']]
            if p['type'] != 'boolean':
                command.append('-r')
            if p.get('choices'):
                command += ['-a', shlex.quote(' '.join(p['choices']))]
            if p['type'] == 'path':
                command = [part for part in command if part != '-f']
            lines.append(' '.join(command))
        for p in c['positionals']:
            if p.get('choices'):
                condition = '__cq_has_path ' + shell_lines(c['path'].split())
                lines.append('complete -c cq -f -n ' + shlex.quote(condition) + ' -a ' + shlex.quote(' '.join(p['choices'])))
    return '\n'.join(lines) + '\n'

def go_source():
    completion = {'bash': bash_completion(), 'zsh': zsh_completion(), 'fish': fish_completion()}
    lines = ['// Code generated by specs/cli-v2/generate.py; DO NOT EDIT.', '', 'package cli', '', 'var globalOptions = []ParameterSpec{']
    lines.extend(parameter_go(p).replace('\t\t\t', '\t', 1).replace('\t\t\t\t', '\t\t', 1) for p in GLOBALS)
    lines += ['}', '', 'var catalogue = []CommandSpec{']
    lines.extend(command_go(c) for c in COMMANDS)
    lines += ['}', '', 'var helpByPath = map[string]string{']
    lines.append('\t"": ' + go_quote((BASE / 'help/cq.txt').read_text()) + ',')
    for c in COMMANDS:
        help_file = BASE / ('help/' + c['path'].replace(' ', '-') + '.txt')
        lines.append('\t' + go_quote(c['path']) + ': ' + go_quote(help_file.read_text()) + ',')
    lines += ['}', '', 'var completionByShell = map[string]string{']
    for shell in ('bash', 'zsh', 'fish'):
        lines.append('\t' + go_quote(shell) + ': ' + go_quote(completion[shell]) + ',')
    lines += ['}', '']
    raw = '\n'.join(lines).encode()
    formatted = subprocess.run(['gofmt'], input=raw, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
    if formatted.returncode != 0:
        raise RuntimeError('gofmt failed: ' + formatted.stderr.decode(errors='replace'))
    return formatted.stdout.decode()

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
    parser.add_argument('--go-output',type=Path,help='Generate immutable Go catalogue, help and completion literals at this path.')
    args=parser.parse_args()
    errors,examples=validate()
    for relative,content in render().items():
        dest=BASE/relative
        if args.check:
            if not dest.exists() or dest.read_text()!=content:errors.append(relative+': generated content differs')
        else:
            dest.parent.mkdir(parents=True,exist_ok=True);dest.write_text(content)
    if args.go_output:
        content = go_source()
        dest = args.go_output
        if args.check:
            if not dest.exists() or dest.read_text() != content:
                errors.append(str(dest) + ': generated Go content differs')
        else:
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_text(content)
    if errors:
        print('\n'.join(errors),file=sys.stderr);return 1
    print(f"Validated {len(COMMANDS)} command/group entries, {examples} examples and {len(COMMANDS)+1} exact help pages.")
    return 0
if __name__=='__main__':sys.exit(main())
