// What everyone is doing, under their name (as in the apps): the game they play ("🎮 Playing …"), their table,
// share or the stream they watch. The apps send it on the bus as {"t":"status","s":text} (stamped "from": the uid)
// every minute and when it changes; gone when they stop sending (2.5 minutes). The web client only shows it.
import { S, el, on } from './core.js';

const status = new Map(); // uid → { text, at }
const FRESH = 150_000;

on('bus:status', (m) => {
  if (typeof m.from !== 'string') return;
  const text = String(m.s ?? '').slice(0, 60);
  const old = status.get(m.from)?.text;
  status.set(m.from, { text, at: Date.now() });
  if (status.size > 2000) status.delete(status.keys().next().value);
  if (old !== text) decorate();
});

/** [uid]'s status now ('' when none or stale). */
export function statusOf(uid) {
  const s = status.get(uid);
  return s && Date.now() - s.at < FRESH ? s.text : '';
}

// Under the name of every person row in the tree and the right column (rows drawn by app.js: .mem with a direct .mname)
function decorate() {
  for (const row of document.querySelectorAll('.mem[data-sid]')) {
    const mem = S.members.get(+row.dataset.sid);
    if (!mem) continue;
    const text = statusOf(mem.uid);
    let box = row.querySelector(':scope > .mtext');
    const name = box ? box.querySelector('.mname') : row.querySelector(':scope > .mname');
    if (!name) continue;
    if (!text) { box?.querySelector('.msub.pstatus')?.remove(); continue; }
    if (!box) {
      box = el('span', { class: 'mtext' });
      name.replaceWith(box);
      box.append(name);
    }
    let sub = box.querySelector('.msub.pstatus');
    if (!sub) { sub = el('span', { class: 'msub pstatus' }); box.append(sub); }
    if (sub.textContent !== text) { sub.textContent = text; sub.title = text; }
  }
}
on('app:render', decorate);
// Stale ones go (no message comes when an app closes)
setInterval(() => {
  const now = Date.now();
  let gone = false;
  for (const [uid, s] of status) if (now - s.at >= FRESH) { status.delete(uid); gone = true; }
  if (gone) decorate();
}, 30_000);
