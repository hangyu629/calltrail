#!/usr/bin/env python3
"""Compare a runtime call trace (from tools/recall/instrument) with a graph.json.

usage: compare.py graph.json trace.txt module-path [--show N]

For every executed function F, the nearest *own* function up the stack is its caller.
If non-own frames (std, other modules) sit in between, the call went "via external"
(a callback), which the static graph models only with `registers` edges.
"""
import json, re, sys, collections

graph_path, trace_path, MOD = sys.argv[1:4]
SHOW = int(sys.argv[sys.argv.index('--show') + 1]) if '--show' in sys.argv else 12
g = json.load(open(graph_path))
fns = {f['id'] for f in g['functions']}
edges = collections.defaultdict(list)          # (from,to) -> [edge...]
out = collections.defaultdict(set)
for e in g['edges']:
    edges[(e['from'], e['to'])].append(e)
    out[e['from']].add(e['to'])

def short(pkg):
    return '(root)' if pkg == MOD else pkg[len(MOD) + 1:]

CLOS = re.compile(r'\.(func|gowrap|deferwrap)\d+(\.\d+)*$')
def parse(name, binary=None):
    """runtime function name (optionally 'name@file') -> (kind, id). kind: own | test | skip | ext"""
    file = ''
    if '@' in name:
        name, file = name.rsplit('@', 1)
    if file.endswith('_test.go'):
        return ('test', None)                  # test code is a root, not part of the module graph
    if name.startswith('main.') and binary and not binary.endswith('.test'):     # package main is named just "main" at runtime
        pkg, rest = MOD + '/cmd/' + binary, name[len('main.'):]
    elif not (name.startswith(MOD + '/') or name == MOD):
        return ('ext', None)
    else:
        i = name.rfind('/')
        d = name.find('.', i)
        pkg, rest = name[:d], name[d + 1:]
    rest = re.sub(r'-range\d+', '', rest)
    rest = re.sub(r'\[.*?\]', '', rest)
    if re.match(r'(init(\.|$)|glob\.)', rest) or '..' in rest:
        return ('own', short(pkg) + '.init')   # package init / var-initialiser closures: one node per package
    while True:
        r2 = CLOS.sub('', rest)
        if r2 == rest: break
        rest = r2
    rest = rest.removesuffix('-fm')
    m = re.match(r'\((\*?)([^)]*)\)\.(.+)$', rest)
    if m:
        return ('own', '(%s%s.%s).%s' % (m.group(1), short(pkg), m.group(2), m.group(3)))
    if '.' in rest:
        t, meth = rest.split('.', 1)
        return ('own', '(%s.%s).%s' % (short(pkg), t, meth))
    return ('own', short(pkg) + '.' + rest)

pairs = {}      # (caller, callee) -> via_external
test_called, own_called, generic_pairs = set(), set(), set()   # callees entered from test code / from own code; pairs touching generics
init_called = collections.Counter()   # callee -> times its nearest own frame was a package initialiser
callees = set()
roots = collections.Counter()
for line in open(trace_path):
    line = line.rstrip('\n')
    if line.count('\t') < 2: continue
    binary, callee_n, chain = line.split('\t', 2)
    k, callee = parse(callee_n, binary)
    if k != 'own': continue
    callees.add(callee)
    generic = '[' in callee_n
    via_ext, caller, test = False, None, False
    for fn in (chain.split('|') if chain else []):
        k, cid = parse(fn, binary)
        if k == 'own' and cid == callee and ('[' in fn or generic):
            continue                           # generics: the shape wrapper and the stenciled body are two frames of one function
        if k == 'own': caller = cid; generic = generic or '[' in fn; break
        if k == 'test': test = True; break
        if k == 'skip': init_called[callee] += 1; break
        via_ext = True                         # std / other module / package-init frame in between
    if test:
        test_called.add(callee); continue
    if caller is None:
        roots[callee_n] += 1; continue
    own_called.add(callee)
    if generic: generic_pairs.add((caller, callee))
    pairs[(caller, callee)] = pairs.get((caller, callee), False) or via_ext
    # keep the *direct* observation if the same pair was seen both ways
    if not via_ext: pairs[(caller, callee)] = False

# callees that have an implicit / reflective / explicit registration edge from *some* function:
# for calls made by external code, the graph draws the edge at the registering function, which is
# not necessarily the nearest own frame on the stack
registered_callee = {e['to'] for e in g['edges'] if e['kind'] == 'registers'}
registered_callee |= {f['id'] for f in g['functions'] if f.get('implicit_callers')}   # edges thinned out, count kept

# a body-less //go:linkname stub never shows up in a stack: caller -> stub -> target counts as caller -> target
via_stub = collections.defaultdict(set)
for e in g['edges']:
    if 'linkname' in e.get('flags', []):
        via_stub[e['to']].add(e['from'])

res = collections.defaultdict(list)
for (a, b), ve in sorted(pairs.items()):
    if a not in fns or b not in fns:
        res['node-not-in-graph'].append((a, b, ve)); continue
    es = edges.get((a, b))
    if not es:
        for stub in via_stub.get(b, ()):
            if (a, stub) in edges: es = edges[(a, stub)]; break
    kind = 'via-external' if ve else 'direct-frame'
    if es:
        low = all(e.get('flags') for e in es)
        res[f'{kind}: found' + (' (only as low-confidence, hidden by default)' if low else '')].append((a, b, ve))
    elif ve and b in registered_callee:
        res['via-external: pair missing, but callee has a registers edge from another function'].append((a, b, ve))
    else:
        res[f'{kind}: MISSING'].append((a, b, ve))

# which kind of static edge confirms each found pair
kindhist = collections.Counter()
for (a, b), ve in pairs.items():
    es = edges.get((a, b))
    if es:
        ks = {e['kind'] for e in es}
        kindhist['direct' if 'direct' in ks else 'dynamic' if 'dynamic' in ks else 'callback' if 'callback' in ks else 'registers'] += 1
total = len(pairs)
print('confirmed by static edge kind:', dict(kindhist))
print(f'runtime call pairs: {total}   executed functions: {len(callees)}   goroutine/main roots: {len(roots)}')
for k in sorted(res):
    print(f'  {len(res[k]):5d}  {k}')

def recall(kinds):
    n = sum(len(v) for k, v in res.items() if any(k.startswith(x) for x in kinds))
    f = sum(len(v) for k, v in res.items() if any(k.startswith(x) for x in kinds) and 'MISSING' not in k)
    return f, n
f, n = recall(['direct-frame'])
print(f'recall, direct frames: {f}/{n} = {100*f/max(n,1):.1f}%')
cov = len(res.get('via-external: pair missing, but callee has a registers edge from another function', []))
fe, ne = recall(['via-external'])     # fe already counts the "covered" pairs as found
print(f'recall, via external (callee covered by some registers edge): {fe}/{ne} = {100*fe/max(ne,1):.1f}%   ({fe - cov} exact + {cov} covered)')

# reachability from every executed main
mains = [m for m in fns if m.endswith('.main') and m.startswith('cmd/')]
executed_mains = [m for m in mains if m in callees or any(m == a for a, _ in pairs)]
inits = [f for f in fns if f.endswith('.init')]          # package initialisation runs before main
gen_called = [f['id'] for f in g['functions'] if f.get('role') == 'generated']   # callers are generated code, not drawn
seen = set(); q = list(executed_mains) + inits + gen_called
while q:
    u = q.pop()
    if u in seen: continue
    seen.add(u); q.extend(out.get(u, ()))
exe = {c for c in callees if c in fns}
unreach = sorted(exe - seen)
print(f'\nexecuted functions in graph: {len(exe)}; reachable from the executed mains {executed_mains} + {len(inits)} init nodes: {len(exe & seen)}; NOT reachable: {len(unreach)}')

only_init = sorted(c for c in init_called if not any(b == c for (_, b) in pairs))
print(f'executed functions whose only observed caller is a package initialiser: {len(only_init)}', only_init[:8])
# sanity check of the roles: a function that actually ran should not be labelled "unused"
role_of = {f['id']: f.get('role', '') for f in g['functions']}
by_role = collections.Counter(role_of[c] or '-' for c in callees if c in role_of)
total_role = collections.Counter(r or '-' for r in role_of.values())
print('executed functions by role (executed / all in graph):')
for r in sorted(total_role): print(f'  {r:11s} {by_role.get(r, 0):4d} / {total_role[r]}')
ran_unused = sorted(c for c in callees if role_of.get(c) == 'unused' and c not in test_called - own_called)
test_only = sorted(c for c in callees if role_of.get(c) == 'unused' and c in test_called - own_called)
print('executed but labelled unused:', ran_unused)
print(f'  (not counted: {len(test_only)} labelled unused whose only observed callers are test code)')
gp = [(p, ve) for p, ve in pairs.items() if p in generic_pairs]
gfound = sum(1 for (a, b), ve in gp if (a, b) in edges or any((a, st) in edges for st in via_stub.get(b, ())))
print(f'generic-related runtime pairs found in graph: {gfound}/{len(gp)}')
for (a, b), ve in gp:
    if not ((a, b) in edges or any((a, st) in edges for st in via_stub.get(b, ()))): print('   MISSING(generic):', a, '->', b, '(via external)' if ve else '')
# executed functions that the graph gives a "nobody calls it" role: who really called them?
callers_of = collections.defaultdict(set)
for (a, b), ve in pairs.items(): callers_of[b].add((a, ve))
for role in ('iface', 'api'):
    ran = sorted(c for c in callees if role_of.get(c) == role)
    print(f'executed functions with role {role}: {len(ran)}')
    for c in ran[:SHOW]:
        who = '; '.join(f"{a}{' (via external)' if ve else ''}" for a, ve in sorted(callers_of.get(c, []))[:2]) or ('test code' if c in test_called else '-')
        print(f'    {c}   <- {who}')
json.dump({k: v for k, v in res.items()}, open(trace_path + '.result.json', 'w'), indent=1)
for k in sorted(res):
    if 'MISSING' in k or k == 'node-not-in-graph':
        print(f'\n--- {k} (first {SHOW})')
        for a, b, ve in res[k][:SHOW]:
            print('  ', a, '->', b)
if unreach:
    print('\n--- executed but unreachable from main (first %d)' % SHOW)
    for u in unreach[:SHOW]: print('  ', u)
