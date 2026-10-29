// Avatars as the PC draws them (Theme.kt Initials): the person's picture from their profile ("av", a file on this
// server only: no other host learns who looks), else a little ghost in their colour (the profile's "hue", else one
// from their identity), whose mouth opens while they talk. Profiles come from the bus ("profiles", kept).

import { on, emit } from './core.js';
import { busKept } from './bus.js';

// Kotlin/Java String.hashCode: the PC's colour for an identity without a chosen one
export function javaHash(s) {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (Math.imul(31, h) + s.charCodeAt(i)) | 0;
  return h;
}
export function hueOf(uid, name) {
  const p = busKept('profiles')[uid];
  const h = Number(p?.hue);
  if (Number.isFinite(h)) return ((h % 360) + 360) % 360;
  return ((javaHash(uid || name || '') & 0x7fffffff) % 360);
}
// Color.hsv(h, s, v) → "rgb(…)" with alpha
function hsv(h, s, v) {
  const f = (n) => { const k = (n + h / 60) % 6; return v - v * s * Math.max(0, Math.min(k, 4 - k, 1)); };
  return [f(5), f(3), f(1)].map((x) => Math.round(x * 255));
}
const rgba = (c, a) => `rgba(${c[0]},${c[1]},${c[2]},${a})`;
const mix = (c, w) => c.map((x) => Math.round(x + (255 - x) * w));

// The picture's path on this server (/f/<id>/<name>), or '' for anything else
export function avatarPath(uid) {
  const av = busKept('profiles')[uid]?.av;
  if (typeof av !== 'string') return '';
  const m = /^https?:\/\/([^/\s]+)\/f\/([0-9a-f]{32})\/([^/\s?#]+)$/.exec(av);
  if (!m || m[1].toLowerCase() !== location.host.toLowerCase()) return '';
  return '/f/' + m[2] + '/' + m[3];
}

const NS = 'http://www.w3.org/2000/svg';
let ids = 0;
function svgEl(tag, attrs, ...kids) {
  const e = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  e.append(...kids);
  return e;
}
function ghost(c) {
  const g1 = 'avb' + (++ids);
  const g2 = 'avg' + ids;
  return svgEl('svg', { viewBox: '0 0 100 100', class: 'ghost', 'aria-hidden': 'true' },
    svgEl('defs', {},
      svgEl('linearGradient', { id: g1, x1: '0', y1: '0', x2: '1', y2: '1' },
        svgEl('stop', { offset: '0', 'stop-color': rgba(c, 0.55) }), svgEl('stop', { offset: '1', 'stop-color': rgba(c, 0.25) })),
      svgEl('linearGradient', { id: g2, x1: '0', y1: '0', x2: '0', y2: '1' },
        svgEl('stop', { offset: '0', 'stop-color': rgba(mix(c, 0.6), 1) }), svgEl('stop', { offset: '1', 'stop-color': rgba(c, 1) }))),
    svgEl('circle', { cx: 50, cy: 50, r: 50, fill: `url(#${g1})` }),
    svgEl('path', { d: 'M20 46C20 10 80 10 80 46L80 90Q70 80 60 90Q50 92 40 90Q30 80 20 90Z', fill: `url(#${g2})` }),
    svgEl('circle', { cx: 41, cy: 42, r: 5.5, fill: '#101020' }),
    svgEl('circle', { cx: 59, cy: 42, r: 5.5, fill: '#101020' }),
    svgEl('ellipse', { class: 'mouth', cx: 50, cy: 57.5, rx: 8, ry: 1.5, fill: '#101020' }));
}

// avatar(name, uid, px): a round <span class="av"> of px pixels
export function avatar(name, uid, px) {
  const e = document.createElement('span');
  e.className = 'av';
  e.dataset.uid = uid || '';
  e.style.width = e.style.height = px + 'px';
  const pic = avatarPath(uid);
  if (pic) {
    const img = document.createElement('img');
    img.className = 'avimg';
    img.alt = '';
    img.src = pic;
    img.loading = 'lazy';
    img.decoding = 'async';
    img.onerror = () => img.replaceWith(ghost(hsv(hueOf(uid, name), 0.6, 0.85)));
    e.append(img);
  } else {
    e.append(ghost(hsv(hueOf(uid, name), 0.6, 0.85)));
  }
  return e;
}

// The server's square with its initials (the PC's ServerIcon)
export function initials(name) {
  const w = String(name || '?').trim().split(/\s+/).filter(Boolean);
  const s = w.length > 1 ? w[0][0] + w[1][0] : (w[0] || '?').slice(0, 2);
  return s.toUpperCase();
}

// A profile changed (a colour, a picture): drawn again
on('bus:kept', ({ kind }) => { if (!kind || kind === 'profiles') emit('app:dirty'); });
