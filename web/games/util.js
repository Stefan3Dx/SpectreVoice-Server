// Small helpers the games' rules share (no DOM): Kotlin-like number reading, and the randomness.

// A JSON number as Kotlin's (x as Number).toInt() reads it: truncated, clamped to an Int; null when it isn't one.
export function toInt(x) {
  if (typeof x !== 'number' || Number.isNaN(x)) return null;
  if (x >= 2147483647) return 2147483647;
  if (x <= -2147483648) return -2147483648;
  return Math.trunc(x);
}
// (x as Number).toInt(), throwing like the cast does when it isn't a number
export function mustInt(x) {
  const v = toInt(x);
  if (v === null) throw new TypeError('not a number');
  return v;
}
// (m[k] as? Number)?.toInt() ?: d
export const intOr = (x, d) => { const v = toInt(x); return v === null ? d : v; };
export function mustList(x) {
  if (!Array.isArray(x)) throw new TypeError('not a list');
  return x;
}
export function mustMap(x) {
  if (!x || typeof x !== 'object' || Array.isArray(x)) throw new TypeError('not a map');
  return x;
}
export const ints = (x) => mustList(x).map(mustInt);
export const bools = (x) => mustList(x).map((v) => v === true);

// Random integers in [lo, hi): crypto when there is one (the browser), else Math.random (tests pass their own)
function cryptoInt(n) {
  const c = globalThis.crypto;
  if (c && c.getRandomValues) {
    const lim = Math.floor(0x100000000 / n) * n;
    const a = new Uint32Array(1);
    for (;;) { c.getRandomValues(a); if (a[0] < lim) return a[0] % n; }
  }
  return Math.floor(Math.random() * n);
}
export const defaultRnd = { nextInt: (lo, hi) => (hi === undefined ? cryptoInt(lo) : lo + cryptoInt(hi - lo)) };

// A seeded generator for the tests (mulberry32)
export function seeded(seed) {
  let a = seed >>> 0;
  const next = () => {
    a = (a + 0x6D2B79F5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
  return { nextInt: (lo, hi) => (hi === undefined ? Math.floor(next() * lo) : lo + Math.floor(next() * (hi - lo))) };
}

export function shuffled(list, rnd = defaultRnd) {
  const a = list.slice();
  for (let i = a.length - 1; i > 0; i--) {
    const j = rnd.nextInt(i + 1);
    [a[i], a[j]] = [a[j], a[i]];
  }
  return a;
}
