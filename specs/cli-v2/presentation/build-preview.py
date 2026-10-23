#!/usr/bin/env python3
"""Build a self-contained review viewer; no external assets or live CQ calls."""
import html,json,pathlib,re
ROOT=pathlib.Path(__file__).resolve().parent

def inline(s):
 s=html.escape(s)
 s=re.sub(r'`([^`]+)`',r'<code>\1</code>',s)
 s=re.sub(r'\*\*([^*]+)\*\*',r'<strong>\1</strong>',s)
 return s

def terminal_line(line):
 # Preview annotations only. Product renderers must assign roles from typed fields.
 def paint(value, role):
  return '<span class="'+role+'">'+html.escape(value)+'</span>'
 success={'complete','completed','changed','saved','passed','healthy','refreshed','available','published','written','restored'}
 warning={'partial','incomplete','pending','unknown','stale','interrupted','completion unknown','verification incomplete','login required','unavailable','unconfirmed'}
 failure={'failed','error','conflicted','rejected'}
 neutral={'none','not configured','not applicable','disabled','enabled','active','inactive','not active','yes','no','—','not established'}
 def value_role(value):
  lower=value.lower()
  if lower in success:return 'success'
  if lower in warning:return 'warning'
  if lower in failure:return 'failure'
  if lower in {'enabled','disabled','active','inactive','not active','yes','no'}:return 'syntax'
  if lower in neutral:return 'secondary'
  return None
 def tokens(value):
  pattern=r'(--[a-z][a-z-]*|(?<!\w)-[hjv](?!\w)|[\w.+-]+@[\w.-]+|(?<![\w.-])[+−-]?\d[\d,]*(?:\.\d+)?(?:%|pp|ms|[smhdx])?(?![\w.-]))'
  parts=[];last=0
  for match in re.finditer(pattern,value):
   parts.append(html.escape(value[last:match.start()]));token=match[0]
   role='syntax' if token.startswith('--') or token in {'-h','-j','-v'} or '@' in token else 'quantity'
   parts.append(paint(token,role));last=match.end()
  parts.append(html.escape(value[last:]));return ''.join(parts)
 # Frozen streams remain colourless, including their embedded numbers and identifiers.
 if line.startswith(('{','[')) or re.match(r'^\d{4}-\d{2}-\d{2}T',line):return html.escape(line)
 if re.match(r'^\s*cq(?: |$)',line):return paint(line,'syntax')
 if line in {'Usage','Arguments','Options','Global options','Related commands','Examples','Commands','Quotas','Providers','Operations','Utilities'}:
  return '<strong>'+html.escape(line)+'</strong>'
 for prefix,role in [('Error ·','failure'),('Warning ·','warning')]:
  if line.startswith(prefix):return '<strong class="'+role+'">'+html.escape(prefix)+'</strong>'+html.escape(line[len(prefix):])
 if ' · ' in line and not line.startswith(' '):
  parts=line.split(' · ')
  result='<strong>'+tokens(parts[0])+'</strong>'
  for part in parts[1:]:
   role=value_role(part)
   result+=paint(' · ','secondary')+(paint(part,role) if role else tokens(part))
  return result
 field=re.match(r'^(\s+)([^:]+?)(:\s+| {2,})(\S.*)$',line)
 if field:
  indent,label,separator,value=field.groups();lower=label.lower().strip()
  role=value_role(value)
  if role is None:
   if lower in {'granted','expires','reset','observed','started','ended','last observed day','last seen','use at','use by','horizon','reset before','reset after','cached observation','deadline','duration','elapsed','period before','period after'}:role='time'
   elif lower in {'reference','account id','account key','account','credit','window','run','attempt','code','command','pid','port','address','digest','receipt digest','promotion digest','fixture sha-256','readiness fingerprint','active release','target release','effect receipt','authority generation','routing generation','effective generation','generation','parser schema','lease schema','schema','cq build','client build','type','tier'}:role='syntax'
  return html.escape(indent)+paint(label,'secondary')+html.escape(separator)+(paint(value,role) if role else tokens(value))
 if re.fullmatch(r'\s*[\w.+-]+@[\w.-]+',line):return paint(line,'syntax')
 return tokens(line)

def render(s):
 out=[];code=False;table=False
 for line in s.splitlines():
  if line.startswith('```'):
   if table:out.append('</table>');table=False
   out.append('</code></pre>' if code else '<pre><code>');code=not code;continue
  if code:out.append(terminal_line(line)+'\n');continue
  if line.startswith('|'):
   if re.match(r'^\|[\s:|\-]+$',line):continue
   if not table:out.append('<table>');table=True
   out.append('<tr>'+''.join('<td>'+inline(c.strip())+'</td>' for c in re.split(r'\|(?=(?:[^`]*`[^`]*`)*[^`]*$)', line.strip('|')))+'</tr>');continue
  if table:out.append('</table>');table=False
  m=re.match(r'^(#{1,6}) (.*)',line)
  if m:out.append('<h{0}>{1}</h{0}>'.format(len(m[1]),inline(m[2])))
  elif line:out.append('<p>'+inline(line)+'</p>')
 if table:out.append('</table>')
 return ''.join(out)

examples={}
help_examples={}
output_cases={}
operational_chapters=[]
for name in ('groups.md','accounts.md','routing.md','runtime.md'):
 path=ROOT/'examples'/name
 if not path.exists():continue
 for chunk in re.split(r'(?=^## )',path.read_text(),flags=re.M):
  if chunk.startswith('## '):
   key=chunk.splitlines()[0][3:].replace('`','')
   parts=re.split(r'(?=^### )',chunk,flags=re.M)
   help_parts=[part for part in parts if part.startswith('### Help')]
   actual_parts=[part for part in parts if part.startswith('### ') and not part.startswith('### Help')]
   help_examples[key]=render('\n'.join(help_parts))
   examples[key]=render('\n'.join(actual_parts))
   output_cases[key]=[{'name':part.splitlines()[0][4:],'html':render(part)} for part in actual_parts]
   if name!='groups.md':operational_chapters.append(parts[0].splitlines()[0]+'\n\n'+'\n'.join(actual_parts))
   if key=='cq completion':
    for shell in ('bash','zsh','fish'):
     raw=ROOT/'examples'/'raw'/('completion.'+shell)
     if raw.exists():
      full='<details><summary>'+shell+' · complete frozen output</summary><pre><code>'+html.escape(raw.read_text())+'</code></pre></details>'
      examples[key]+=full
      for case in output_cases[key]:
       if shell.lower() in case['name'].lower():case['html']+=full

sections=[{'name':'Colour system','html':render((ROOT/'colour.md').read_text())}]
for name in ('README.md','groups.md','accounts.md','routing.md','runtime.md'):
 text=(ROOT/name).read_text()
 if name=='README.md':sections.append({'name':'Design system','html':render(text)});continue
 chunks=re.split(r'(?=^## )',text,flags=re.M)
 for chunk in chunks:
  if chunk.startswith('## '):
   title=chunk.splitlines()[0][3:].replace('`','')
   example_key='cq --help' if name=='groups.md' and title=='cq help' else title
   sections.append({'name':title if example_key!='cq --help' else 'cq — root help','html':render(chunk),'examples':examples.get(example_key),'help':help_examples.get(example_key),'cases':output_cases.get(example_key,[]),'group':name=='groups.md','command':example_key})
(ROOT/'examples'/'operational-outputs.md').write_text('# CQ operational output examples\n\nHand-authored output examples for all 89 commands, collected without their help pages. These are proposed terminal designs using fictional data. Each scenario records invocation, output streams and exit status. Edit the owning accounts/routing/runtime document, then rebuild this collection.\n\n'+'\n\n'.join(operational_chapters))
blob=json.dumps(sections).replace('<','\\u003c')
page='''<!doctype html><html lang="en"><meta charset="utf-8"><title>CQ command design catalogue</title><style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#0c121c;color:#e2e9f2;font:15px/1.6 system-ui}aside{position:fixed;inset:0 auto 0 0;width:290px;padding:20px;background:#141e2d;overflow:auto}input{width:100%;padding:10px;background:#0c121c;color:#edf5ff;border:1px solid #536176;border-radius:5px}button{display:block;text-align:left;width:100%;border:0;background:transparent;color:#b8c6d9;padding:7px;cursor:pointer}button:hover,button.active{background:#253449;color:#78d8e9}main{margin-left:290px;padding:36px;max-width:1280px}h1,h2{color:#78d8e9}h3{margin-top:32px}code{font-family:ui-monospace,Menlo,monospace;color:#9adddf}#scenarios{margin:0 0 20px}select{background:#141e2d;color:#e2e9f2;border:1px solid #536176;padding:8px;border-radius:5px}pre code{color:inherit}.syntax{color:#78d8e9}.quantity{color:#a9c7ff;font-weight:600}.time{color:#d7b5f7}.secondary{color:#a5b4c8}.success{color:#9ad590}.warning{color:#f0c36e}.failure{color:#ff9191}body.plain pre span,body.plain pre strong{color:inherit}#tabs{display:flex;gap:8px;margin-bottom:24px}#tabs button{width:auto;border:1px solid #536176;border-radius:5px}pre{white-space:pre-wrap;background:#141e2d;padding:20px;border-radius:8px;line-height:1.55}table{border-collapse:collapse;width:100%;font-size:13px}td{padding:8px;border-bottom:1px solid #304055;vertical-align:top;overflow-wrap:anywhere}p{overflow-wrap:anywhere}.muted{color:#a5b4c8;font-size:13px}@media(max-width:750px){aside{position:static;width:auto;max-height:260px}main{margin:0;padding:20px}}
</style><aside><b>CQ · design catalogue</b><p class="muted">Draft · proposed layouts, not live output</p><p class="muted"><span class="syntax">Identity / syntax</span> · <span class="quantity">quantity</span> · <span class="time">time</span><br><span class="success">confirmed</span> · <span class="warning">attention</span> · <span class="failure">failure</span></p><label class="muted"><input type="checkbox" id="plain" style="width:auto"> No colour in examples</label><input id="q" placeholder="Find command…" aria-label="Find command"><div id="nav"></div></aside><main><div id="tabs"></div><div id="scenarios"></div><div id="content"></div></main><script>
const pages=DATA;const nav=document.querySelector('#nav'), content=document.querySelector('#content'),q=document.querySelector('#q'),tabs=document.querySelector('#tabs'),scenarios=document.querySelector('#scenarios');let selected=0,mode='output',variant=0;
function choose(i,keep=false){if(i!==selected&&!keep){variant=0;mode=pages[i].group?'help':'output'}selected=i;const p=pages[i];tabs.replaceChildren();scenarios.replaceChildren();
if(p.examples){for(const [value,label] of [['output',p.group?'Invalid command':'Output'],['help','Help'],['spec','Specification']]){let b=document.createElement('button');b.textContent=label;b.className=mode===value?'active':'';b.onclick=()=>{mode=value;choose(selected,true)};tabs.append(b)}}
if(mode==='output'&&p.cases&&p.cases.length){const label=document.createElement('label');label.textContent='Scenario ';const select=document.createElement('select');select.setAttribute('aria-label','Output scenario');p.cases.forEach((c,j)=>{let option=document.createElement('option');option.value=j;option.textContent=c.name;select.append(option)});variant=Math.min(variant,p.cases.length-1);select.value=variant;select.onchange=()=>{variant=Number(select.value);choose(selected,true)};label.append(select);scenarios.append(label);content.innerHTML=p.cases[variant].html}
else content.innerHTML=mode==='help'&&p.help?p.help:p.html;
if(p.command)history.replaceState(null,'','#'+encodeURIComponent(p.command));draw();window.scrollTo(0,0)}
function draw(){nav.replaceChildren();pages.forEach((p,i)=>{if(!p.name.toLowerCase().includes(q.value.toLowerCase()))return;let b=document.createElement('button');b.textContent=p.name;b.className=i===selected?'active':'';b.onclick=()=>choose(i);nav.append(b)})}
q.oninput=draw;document.querySelector('#plain').onchange=e=>document.body.classList.toggle('plain',e.target.checked);const requested=decodeURIComponent(location.hash.slice(1));const initial=pages.findIndex(p=>p.command===requested);choose(initial>=0?initial:pages.findIndex(p=>p.command==='cq codex reset list'));
</script></html>'''.replace('DATA',blob)
(ROOT/'preview.html').write_text(page)
print(f'Built preview with {len(sections)} sections.')
