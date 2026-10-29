# Packs the Kotlin recording for the repo: each step's "in" is the last state taken (dropped, rebuilt by the replay),
# a chess history only its new move ({"push": m}), a blackjack shoe only how many cards are left of the one before
# ({"keep": n}).
import json, sys
rec = json.load(open(sys.argv[1]))
taken = 0
for g in rec['games']:
    prev = {'state': g['start']['state'], 'secret': g['start']['secret']}
    for s in g['steps']:
        assert s['in'] == prev, (g['kind'], 'in')
        del s['in']
        o = s['out']
        if o is None:
            continue
        taken += 1
        st = dict(o['state'])
        nxt = {'state': dict(st, final=True) if o['final'] else st, 'secret': o['secret'] if o['secret'] is not None else prev['secret']}
        ph = prev['state'].get('hist')
        if g['kind'] == 'CHESS' and isinstance(ph, list) and o['state']['hist'][:-1] == ph:
            o['state'] = dict(o['state'], hist={'push': o['state']['hist'][-1]})
        if g['kind'] == 'BLACKJACK' and o['secret'] is not None and prev['secret'] is not None:
            a, b = prev['secret']['shoe'], o['secret']['shoe']
            if b == a[:len(b)]:
                o['secret'] = dict(o['secret'], shoe={'keep': len(b)})
        prev = nxt
json.dump(rec, open(sys.argv[2], 'w'), separators=(',', ':'), ensure_ascii=False)
print('taken', taken)
