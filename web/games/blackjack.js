// Blackjack against the house, as Blackjack.kt: the keeper deals from six decks; hit / stand / double; the dealer
// draws to 17 (stands on a soft 17). Blackjack pays 3:2, a win 1:1, a tie keeps the bet. The shoe (and the hole card
// while the players play) stays in the keeper's secret; everyone else gets public().

import { mustInt, ints, bools, mustList, defaultRnd, shuffled } from './util.js';

export const DECKS = 6;
const newShoe = (rnd) => shuffled([...Array(52 * DECKS).keys()].map((i) => i % 52), rnd);

export const rank = (card) => (card % 13) + 1;
export const suit = (card) => Math.floor(card / 13);

export function value(cards) {
  const v = cards.reduce((a, c) => a + Math.min(rank(c), 10), 0);
  const ace = cards.some((c) => rank(c) === 1);
  if (ace && v + 10 <= 21) return [v + 10, true];
  return [v, false];
}
export const blackjack = (cards) => cards.length === 2 && value(cards)[0] === 21;

export function start(players, rnd = defaultRnd) {
  return {
    shoe: newShoe(rnd), dealer: [], hands: Array.from({ length: players }, () => []), bets: Array(players).fill(0), ready: Array(players).fill(false),
    done: Array(players).fill(false), phase: 'bet', turn: -1, result: Array(players).fill(0), outcome: Array(players).fill(''), round: 1, left: Array(players).fill(false),
  };
}

export function decode(m) {
  const hands = mustList(m.hands).map(ints);
  if (typeof m.phase !== 'string') throw new TypeError('phase');
  return {
    shoe: ints(m.shoe), dealer: ints(m.dealer), hands, bets: ints(m.bets), ready: bools(m.ready), done: bools(m.done), phase: m.phase, turn: mustInt(m.turn),
    result: ints(m.result), outcome: mustList(m.outcome).map((x) => (typeof x === 'string' ? x : '')), round: mustInt(m.round),
    left: Array.isArray(m.left) ? bools(m.left) : Array(hands.length).fill(false),
  };
}

export function encode(s) {
  return {
    shoe: s.shoe.slice(), dealer: s.dealer.slice(), hands: s.hands.map((h) => h.slice()), bets: s.bets.slice(), ready: s.ready.slice(), done: s.done.slice(),
    phase: s.phase, turn: s.turn, result: s.result.slice(), outcome: s.outcome.slice(), round: s.round, left: s.left.slice(),
  };
}

// What the others may see: the shoe stays with the dealer, and the hole card until the dealer plays
// ("left" is the cards left in the shoe here, as the Kotlin keeper sends it)
export function publicState(s) {
  return { ...encode(s), shoe: [], left: s.shoe.length, dealer: s.phase === 'play' ? [...s.dealer.slice(0, 1), -1] : s.dealer.slice() };
}

// Player p's op ("bet" amount, "skip", "deal", "hit", "stand", "double", "next", "leave"); purse(i) a player's Lei
export function move(s, p, op, amount, purse, host, rnd = defaultRnd) {
  switch (op) {
    case 'leave':
      s.left[p] = true;
      if (s.phase === 'bet') { s.bets[p] = 0; s.ready[p] = true; if (s.ready.every(Boolean) && s.bets.some((b) => b > 0)) deal(s, rnd); }
      else if (s.phase === 'play') { if (!s.done[p]) { s.done[p] = true; if (s.turn === p) advance(s, rnd); } }
      break;
    case 'bet': case 'skip':
      if (s.phase !== 'bet') return 'not betting now';
      if (op === 'bet') {
        if (amount < 1 || amount > purse(p) + s.bets[p]) return 'bet ' + amount;
        s.bets[p] = amount;
      } else s.bets[p] = 0;
      s.ready[p] = true;
      if (s.ready.every(Boolean) && s.bets.some((b) => b > 0)) deal(s, rnd);
      break;
    case 'deal':
      if (s.phase !== 'bet' || !host) return 'not now';
      if (!s.bets.some((b) => b > 0)) return 'nobody bet';
      for (let i = 0; i < s.bets.length; i++) if (!s.ready[i]) s.bets[i] = 0;
      deal(s, rnd);
      break;
    case 'hit': case 'stand': case 'double': {
      if (s.phase !== 'play' || s.turn !== p) return 'not your turn';
      const h = s.hands[p];
      if (op === 'hit') { h.push(draw(s, rnd)); if (value(h)[0] >= 21) s.done[p] = true; }
      else if (op === 'stand') s.done[p] = true;
      else {
        if (h.length !== 2 || purse(p) < s.bets[p]) return "can't double";
        s.bets[p] *= 2;
        h.push(draw(s, rnd));
        s.done[p] = true;
      }
      advance(s, rnd);
      break;
    }
    case 'next':
      if (s.phase !== 'done') return 'not over';
      s.phase = 'bet'; s.round++; s.turn = -1;
      s.dealer = [];
      for (let i = 0; i < s.hands.length; i++) { s.hands[i] = []; s.bets[i] = 0; s.ready[i] = s.left[i]; s.done[i] = false; s.result[i] = 0; s.outcome[i] = ''; }
      break;
    default: return 'what?';
  }
  return null;
}

function draw(s, rnd) {
  if (!s.shoe.length) s.shoe.push(...newShoe(rnd));
  return s.shoe.pop();
}

function deal(s, rnd) {
  if (s.shoe.length < 52) s.shoe = newShoe(rnd);
  const playing = s.bets.map((b, i) => (b > 0 ? i : -1)).filter((i) => i >= 0);
  for (let k = 0; k < 2; k++) {
    for (const i of playing) s.hands[i].push(draw(s, rnd));
    s.dealer.push(draw(s, rnd));
  }
  for (let i = 0; i < s.hands.length; i++) s.done[i] = !playing.includes(i) || blackjack(s.hands[i]);
  s.phase = 'play';
  s.turn = -1;
  if (blackjack(s.dealer)) { finish(s); return; }
  advance(s, rnd);
}

function advance(s, rnd) {
  let next = -1;
  for (let i = 0; i < s.hands.length; i++) if (i > s.turn && !s.done[i]) { next = i; break; }
  if (next < 0) for (let i = 0; i < s.hands.length; i++) if (!s.done[i]) { next = i; break; }
  if (next >= 0) { s.turn = next; return; }
  const alive = s.bets.some((b, i) => b > 0 && value(s.hands[i])[0] <= 21 && !blackjack(s.hands[i]));
  if (alive) while (value(s.dealer)[0] < 17) s.dealer.push(draw(s, rnd));
  finish(s);
}

function finish(s) {
  const d = value(s.dealer)[0];
  const dealerBj = blackjack(s.dealer);
  for (let i = 0; i < s.hands.length; i++) {
    const bet = s.bets[i];
    if (bet <= 0) { s.result[i] = 0; s.outcome[i] = ''; continue; }
    const h = s.hands[i];
    const v = value(h)[0];
    let r;
    if (v > 21) r = [-bet, 'Ai depășit 21'];
    else if (blackjack(h) && !dealerBj) r = [Math.trunc(bet * 3 / 2), 'Blackjack!'];
    else if (dealerBj && !blackjack(h)) r = [-bet, 'Dealerul are blackjack'];
    else if (blackjack(h)) r = [0, 'Egalitate'];
    else if (d > 21) r = [bet, 'Dealerul a depășit'];
    else if (v > d) r = [bet, 'Câștigat'];
    else if (v === d) r = [0, 'Egalitate'];
    else r = [-bet, 'Pierdut'];
    s.result[i] = r[0]; s.outcome[i] = r[1];
  }
  s.phase = 'done';
  s.turn = -1;
}

export const Blackjack = { DECKS, rank, suit, value, blackjack, start, decode, encode, publicState, move };
