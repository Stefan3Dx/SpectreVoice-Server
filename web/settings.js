// Settings (kept in this browser): language, microphone and speakers, the browser's noise suppression, the
// push-to-talk key, and notifications while the tab is hidden (opt-in).

import { el, emit, openDialog } from './core.js';
import { t, LANGS, getLang, setLang } from './i18n.js';
import { prefs, setInputDevice, setOutputDevice, setNoiseSuppression, setPttKey, keyName } from './voice.js';
import { notifyPrefs, setNotify } from './chat.js';

async function devices() {
  try { return await navigator.mediaDevices.enumerateDevices(); } catch { return []; }
}

export async function openSettings() {
  const all = await devices();
  const ins = all.filter((d) => d.kind === 'audioinput');
  const outs = all.filter((d) => d.kind === 'audiooutput');
  const sinkOK = typeof HTMLMediaElement !== 'undefined' && 'setSinkId' in HTMLMediaElement.prototype;
  const devSelect = (id, list, cur, onchange) => {
    const s = el('select', { id }, el('option', { value: '', text: t('systemDefault') }),
      list.filter((d) => d.deviceId && d.deviceId !== 'default').map((d, i) => el('option', { value: d.deviceId, text: d.label || t('device', { n: i + 1 }), selected: d.deviceId === cur })));
    s.onchange = () => onchange(s.value);
    return s;
  };

  const langSel = el('select', { id: 'setLang' }, LANGS.map(([c, n]) => el('option', { value: c, text: n, selected: c === getLang() })));
  langSel.onchange = async () => { await setLang(langSel.value); emit('app:lang'); openSettings(); };

  const ns = el('input', { type: 'checkbox', id: 'setNs', checked: prefs.ns });
  ns.onchange = () => setNoiseSuppression(ns.checked);

  const ptt = el('button', { type: 'button', id: 'setPtt', class: 'keycap', text: keyName(prefs.pttKey) });
  ptt.onclick = () => {
    ptt.textContent = t('pressKey');
    ptt.classList.add('on');
    const grab = (ev) => {
      ev.preventDefault();
      ev.stopPropagation();
      document.removeEventListener('keydown', grab, true);
      ptt.classList.remove('on');
      if (ev.code !== 'Escape') setPttKey(ev.code);
      ptt.textContent = keyName(prefs.pttKey);
    };
    document.addEventListener('keydown', grab, true);
  };

  const notifyOK = 'Notification' in window;
  const notify = el('input', { type: 'checkbox', id: 'setNotify', checked: notifyPrefs.on, disabled: !notifyOK });
  notify.onchange = async () => { notify.checked = await setNotify(notify.checked); };

  const row = (label, control, note) => el('label', { class: 'setrow' }, el('span', { class: 'setlabel' }, label, note ? el('small', { class: 'muted', text: note }) : null), control);
  const body = el('div', { class: 'settings' },
    el('h3', { text: t('language') }),
    row(t('language'), langSel),
    el('h3', { text: t('voice') }),
    row(t('microphone'), devSelect('setIn', ins, prefs.inDev, (v) => setInputDevice(v))),
    sinkOK ? row(t('speakers'), devSelect('setOut', outs, prefs.outDev, (v) => setOutputDevice(v))) : el('p', { class: 'muted small', text: t('speakersNo') }),
    row(t('noiseSuppression'), ns, t('noiseNote')),
    el('div', { class: 'setrow' }, el('span', { class: 'setlabel' }, t('pttKey'), el('small', { class: 'muted', text: t('pttNote') })), ptt),
    el('h3', { text: t('notifications') }),
    row(t('notifyHidden'), notify, notifyOK ? t('notifyNote') : t('notifyNo')));
  openDialog({ title: t('settings'), body, actions: [{ text: t('done'), primary: true }] });
}
