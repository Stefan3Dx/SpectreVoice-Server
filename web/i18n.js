// The page's texts in the apps' ten languages: web/i18n-<lang>.json ({key: text}; "{name}" placeholders), English
// always loaded as the fallback. Elements with data-i18n="key" (text), data-i18n-ph (placeholder) and data-i18n-title
// get theirs on every change. A module with texts of its own adds them with addTexts({en: {…}, ro: {…}, …}).

import { store } from './core.js';

export const LANGS = [
  ['en', 'English'], ['ro', 'Română'], ['es', 'Español'], ['de', 'Deutsch'], ['pl', 'Polski'],
  ['fr', 'Français'], ['it', 'Italiano'], ['pt', 'Português'], ['hu', 'Magyar'], ['tr', 'Türkçe'],
];
const known = new Set(LANGS.map((l) => l[0]));
const tables = { en: {} };
const extra = {};
let lang = 'en';

function guess() {
  const saved = store.get('sv.lang', '');
  if (known.has(saved)) return saved;
  for (const l of navigator.languages || [navigator.language || 'en']) {
    const p = String(l).toLowerCase().slice(0, 2);
    if (known.has(p)) return p;
  }
  return 'en';
}

async function load(l) {
  if (tables[l] && Object.keys(tables[l]).length) return;
  try {
    const r = await fetch('i18n-' + l + '.json', { cache: 'no-cache' });
    tables[l] = r.ok ? await r.json() : {};
  } catch { tables[l] = {}; }
}

export async function initLang() {
  lang = guess();
  await Promise.all([load('en'), load(lang)]);
}
export const getLang = () => lang;
export async function setLang(l) {
  if (!known.has(l)) return;
  await load(l);
  lang = l;
  store.set('sv.lang', l);
  applyTexts();
}

export function addTexts(byLang) {
  for (const [l, tab] of Object.entries(byLang)) extra[l] = { ...(extra[l] || {}), ...tab };
}

export function t(k, vars) {
  const s = tables[lang]?.[k] ?? extra[lang]?.[k] ?? tables.en[k] ?? extra.en?.[k] ?? k;
  return s.replace(/\{(\w+)\}/g, (_, v) => (vars && v in vars ? vars[v] : ''));
}

export function applyTexts(root = document) {
  document.documentElement.lang = lang;
  for (const e of root.querySelectorAll('[data-i18n]')) e.textContent = t(e.dataset.i18n);
  for (const e of root.querySelectorAll('[data-i18n-ph]')) e.placeholder = t(e.dataset.i18nPh);
  for (const e of root.querySelectorAll('[data-i18n-title]')) { e.title = t(e.dataset.i18nTitle); e.setAttribute('aria-label', e.title); }
}

// A server refusal ({"t":"error","op","why","message"}) in words
export function errText(m) {
  const w = m.why;
  if (w === 'password') return m.op === 'hello' ? t('errPassword') : t('errChPassword');
  const k = { banned: 'errBanned', full: 'errFull', many: 'errMany', name: 'errName', auth: 'errAuth', rate: 'errRate', invite: 'errInvite', muted: 'errMuted', role: 'errRole', gone: 'errGone', server: 'errServer' }[w];
  return k ? t(k) : (m.message || w || 'error');
}
