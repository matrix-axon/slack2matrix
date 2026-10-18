"""Check that Slack message links were rewritten to the right Matrix events.

usage: python3 -I scripts/verify-links.py <export dir> <import.db> <dump.jsonl>

The dump comes from `testclient check -dump "*"` (its JSON lines). Exits 1 if
any link is wrong.

For every Slack message that links to another Slack message, the decrypted
Matrix event(s) imported from it must contain a matrix.to link to the event
the importer recorded for the linked message.
"""
import collections, glob, json, os, re, sqlite3, sys, urllib.parse

export, dbpath, dumppath = sys.argv[1:4]
chans = {c['id']: c['name'] for c in json.load(open(os.path.join(export, 'channels.json')))}
pat = re.compile(r'\.slack\.com/archives/([A-Z0-9]+)/p(\d{10})(\d{6})')

def channel_files(name):
    root = os.fsencode(export)
    for entry in os.listdir(root):
        try:
            n = entry.decode('utf-8')
        except UnicodeDecodeError:
            n = entry.decode('latin-1')
        if n == name:
            d = os.path.join(root, entry)
            return [os.fsdecode(os.path.join(d, f)) for f in os.listdir(d)
                    if f.endswith(b'.json') and not f.startswith(b'.') and b'canvas_in' not in f]
    return []

# Slack message -> set of linked (channel, ts)
expected = {}
for cid, name in chans.items():
    for f in channel_files(name):
        for m in json.load(open(f)):
            if 'ts' not in m:
                continue
            m.pop('root', None)  # a thread_broadcast's copy of the root isn't rendered
            targets = {(tc, f'{a}.{b}') for tc, a, b in pat.findall(json.dumps(m).replace('\\/', '/'))}
            targets.discard((cid, m['ts']))
            if targets:
                expected[(cid, m['ts'])] = targets

db = sqlite3.connect(dbpath)
first = {}
for ch, ts, evt in db.execute("SELECT channel_id, slack_ts, event_id FROM events WHERE status='sent' AND kind IN ('message','media') ORDER BY part DESC"):
    first[(ch, ts)] = evt  # lowest part wins (ordered DESC, last write)
rooms = dict(db.execute("SELECT channel_id, room_id FROM rooms"))

link_re = re.compile(r'https://matrix\.to/#/([^"\s<>)?]+)')
got = collections.defaultdict(set)  # Slack message -> set of matrix.to targets in its events
for line in open(dumppath):
    e = json.loads(line)
    src = e['content'].get('net.slack2matrix.source')
    if not src:
        continue
    key = (src['channel'], src['ts'])
    if key not in expected:
        continue
    text = json.dumps(e['content'])
    for t in link_re.findall(text):
        got[key].add(urllib.parse.unquote(t))

c = collections.Counter()
bad = []
for key, targets in expected.items():
    if key not in first:
        c['source message not imported'] += 1
        continue
    for tc, tts in targets:
        room = rooms.get(tc)
        evt = first.get((tc, tts))
        if room is None:
            c['target channel not imported (Slack link kept)'] += 1
        elif evt is None:
            ok = any(t == room for t in got[key])
            c['target message not imported -> room link' + ('' if ok else ' MISSING')] += 1
            if not ok:
                bad.append((key, tc, tts))
        elif f'{room}/{evt}' in got[key]:
            c['linked to the right event'] += 1
        else:
            c['WRONG or missing event link'] += 1
            bad.append((key, tc, tts, sorted(got[key])[:3]))
for k, v in sorted(c.items()):
    print(f'{v:7d}  {k}')
for b in bad:
    print('BAD', b)
sys.exit(1 if bad else 0)
