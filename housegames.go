package main

// The house games the server deals itself (house.go): Go ports of the apps' Slots and Roulette (Casino.kt), Fruits
// (Fruits.kt) and Blackjack (Blackjack.kt), with the money Rules.kt works out for each move. The states are the
// apps' own, JSON for JSON (encode, and Blackjack's public view), so the apps' and the web's views show them as they
// are; the outcome strings stay in Romanian, as the apps write them. The recording of the apps' rules
// (web/games/test/kotlin-games.json.gz, kotlin-fruits.json.gz) is replayed through these in house_test.go.
//
// houseMove reads the state the way Rules.move does (decoded afresh each move: a refused move changes nothing),
// and says, besides Rules' Outcome, how the chips move: "stake" (taken from the purse when they go on the table,
// given back when taken off) and, when a hand or spin is settled, "back" (what comes back: the stake and the win).

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"slices"
)

// houseRand: where the reels, the wheel, the cards come from (crypto; the tests pass their own).
type houseRand interface {
	Intn(n int) int
	Float64() float64
}

type cryptoRand struct{}

func (cryptoRand) u64() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return binary.LittleEndian.Uint64(b[:])
}

func (r cryptoRand) Intn(n int) int {
	if n <= 0 {
		panic("Intn")
	}
	lim := math.MaxUint64 - math.MaxUint64%uint64(n)
	for {
		if v := r.u64(); v < lim {
			return int(v % uint64(n))
		}
	}
}

func (r cryptoRand) Float64() float64 { return float64(r.u64()>>11) / (1 << 53) }

// houseOut: a move taken (Rules.kt's Outcome for these games) and the chips it moves.
type houseOut struct {
	state  map[string]any
	secret map[string]any // Blackjack's whole state (the shoe, the hole card): never leaves the server
	pay    map[int]int    // Outcome.pay: seat → the change to the purse now
	profit map[int]int    // Outcome.profit (nil: none, the pay counts)
	over   []int          // nil: no hand ended
	played []int
	stake  map[int]int // seat → chips put on the table (<0, taken from the purse) or taken off (>0, given back)
	back   map[int]int // settle: seat → what comes back (stake and win; 0: lost)
	settle bool        // a hand or spin was settled: what's left on the table of it is lost
}

// ====== Reading a state as the Kotlin's casts do (a wrong one panics; houseMove refuses the move) ======

// kInt is (x as Number).toInt(): truncated, clamped to an Int; false when it isn't a number.
func kInt(x any) (int, bool) {
	f, ok := x.(float64)
	if !ok {
		if i, isInt := x.(int); isInt {
			return i, true
		}
		return 0, false
	}
	if math.IsNaN(f) {
		return 0, true
	}
	if f >= math.MaxInt32 {
		return math.MaxInt32, true
	}
	if f <= math.MinInt32 {
		return math.MinInt32, true
	}
	return int(f), true
}

func mustInt(x any) int {
	v, ok := kInt(x)
	if !ok {
		panic("not a number")
	}
	return v
}

// intArg is Rules' int(k): (m[k] as? Number)?.toInt() ?: -1.
func intArg(m map[string]any, k string) int {
	if v, ok := kInt(m[k]); ok {
		return v
	}
	return -1
}

func mustList(x any) []any {
	l, ok := x.([]any)
	if !ok {
		panic("not a list")
	}
	return l
}

func mustInts(x any) []int {
	l := mustList(x)
	out := make([]int, len(l))
	for i, v := range l {
		out[i] = mustInt(v)
	}
	return out
}

func mustBools(x any) []bool {
	l := mustList(x)
	out := make([]bool, len(l))
	for i, v := range l {
		out[i] = v == true
	}
	return out
}

func mustString(x any) string {
	s, ok := x.(string)
	if !ok {
		panic("not a string")
	}
	return s
}

// jsonNorm makes v what the bus would carry (maps of any, float64 numbers), so states compare and decode the same.
func jsonNorm(v map[string]any) map[string]any {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func sumInts(l []int) int {
	t := 0
	for _, v := range l {
		t += v
	}
	return t
}

func seatsWhere(n int, f func(int) bool) []int {
	out := []int{}
	for i := 0; i < n; i++ {
		if f(i) {
			out = append(out, i)
		}
	}
	return out
}

// houseKinds: the games the server deals (Rules.house in the apps), and how many seats each has at most.
var houseKinds = map[string]int{"SLOTS": 1, "FRUITS": 1, "ROULETTE": 8, "BLACKJACK": 6}

// houseStart: a new game of kind for n players (Rules.start): the state everyone sees, and the secret.
func houseStart(kind string, n int, rnd houseRand) (st, sec map[string]any) {
	switch kind {
	case "SLOTS":
		return jsonNorm(slotsEncode(slotsStart())), nil
	case "FRUITS":
		return jsonNorm(fruitsEncode(fruitsStart())), nil
	case "ROULETTE":
		return jsonNorm(rouEncode(rouStart(n))), nil
	case "BLACKJACK":
		s := bjStart(n, rnd)
		return jsonNorm(bjPublic(s)), jsonNorm(bjEncode(s))
	}
	panic("no house game " + kind)
}

// houseMove: seat p's move m on the table (Rules.move); nil when it isn't allowed. purse(i): what seat i may stake.
func houseMove(kind string, st, sec map[string]any, p int, m map[string]any, host bool, purse func(int) int, rnd houseRand) (o *houseOut) {
	defer func() {
		if r := recover(); r != nil {
			o = nil
		}
	}()
	op, _ := m["op"].(string)
	switch kind {
	case "SLOTS":
		return slotsMove(st, p, op, m, purse, rnd)
	case "FRUITS":
		return fruitsMove(st, p, op, m, purse, rnd)
	case "ROULETTE":
		return rouMove(st, p, op, m, host, purse, rnd)
	case "BLACKJACK":
		if sec == nil {
			return nil
		}
		return bjMove(sec, p, op, m, host, purse, rnd)
	}
	return nil
}

// houseAuto: what's played for a seat out of time (Rules.autoMoves), tried in order: the quietest move there is.
func houseAuto(kind string) []map[string]any {
	switch kind {
	case "BLACKJACK":
		return []map[string]any{{"op": "skip"}, {"op": "stand"}}
	case "ROULETTE":
		return []map[string]any{{"op": "ready"}}
	case "FRUITS":
		return []map[string]any{{"op": "collect"}}
	}
	return nil
}

// ====== Slots: three reels; three alike pays big, two alike gives the stake back ======

var (
	slotsWeights = []int{300, 250, 200, 120, 70, 40, 15, 5}
	slotsThree   = []int{6, 8, 12, 16, 25, 50, 150, 500}
)

const slotsMaxStake = 2000

type slotsState struct {
	reels              []int
	stake, last, spins int
	history            []int
}

func slotsStart() *slotsState {
	return &slotsState{reels: []int{0, 1, 2}, stake: 10, history: []int{}}
}

func slotsEncode(s *slotsState) map[string]any {
	return map[string]any{"reels": s.reels, "stake": s.stake, "last": s.last, "spins": s.spins, "history": s.history}
}

func slotsDecode(m map[string]any) *slotsState {
	return &slotsState{reels: mustInts(m["reels"]), stake: mustInt(m["stake"]), last: mustInt(m["last"]), spins: mustInt(m["spins"]), history: mustInts(m["history"])}
}

func slotsPick(rnd houseRand) int {
	r := rnd.Intn(sumInts(slotsWeights))
	for i, w := range slotsWeights {
		if r < w {
			return i
		}
		r -= w
	}
	return 0
}

// slotsPayout: all a spin of reels gives back for stake (0: lost).
func slotsPayout(reels []int, stake int) int {
	a, b, c := reels[0], reels[1], reels[2]
	switch {
	case a == b && b == c:
		return stake * slotsThree[a]
	case a == b || b == c || a == c:
		return stake
	}
	return 0
}

func slotsMove(st map[string]any, p int, op string, m map[string]any, purse func(int) int, rnd houseRand) *houseOut {
	if op != "spin" {
		return nil
	}
	s := slotsDecode(st)
	stake := intArg(m, "v")
	if stake < 1 || stake > purse(p) || stake > slotsMaxStake {
		return nil
	}
	s.reels = []int{slotsPick(rnd), slotsPick(rnd), slotsPick(rnd)}
	s.stake = stake
	won := slotsPayout(s.reels, stake)
	s.last = won - stake
	s.spins++
	s.history = append([]int{s.last}, s.history...)
	if len(s.history) > 10 {
		s.history = s.history[:10]
	}
	o := &houseOut{state: jsonNorm(slotsEncode(s)), pay: map[int]int{}, over: []int{}, played: []int{p},
		stake: map[int]int{p: -stake}, back: map[int]int{p: won}, settle: true}
	if s.last != 0 {
		o.pay[p] = s.last
	}
	if s.last > 0 {
		o.over = []int{p}
	}
	return o
}

// ====== Fruits: 5 reels of 3, 10 lines, a wild, a scatter, stacked reels, and the gamble ======

const (
	fruitsWild        = 8
	fruitsScatter     = 9
	fruitsReels       = 5
	fruitsRows        = 3
	fruitsGambleSteps = 5
	fruitsGambleMax   = 100_000
	fruitsWinCap      = 1_000_000
	fruitsStacked     = 0.1
)

var (
	fruitsSymbols  = 10
	fruitsLineBets = []int{1, 2, 5, 10, 20, 50, 100}
	fruitsLines    = [][]int{
		{1, 1, 1, 1, 1}, {0, 0, 0, 0, 0}, {2, 2, 2, 2, 2}, {0, 1, 2, 1, 0}, {2, 1, 0, 1, 2},
		{1, 0, 0, 0, 1}, {1, 2, 2, 2, 1}, {0, 0, 1, 2, 2}, {2, 2, 1, 0, 0}, {1, 2, 1, 0, 1},
	}
	fruitsPay = map[int]map[int]int{
		0: {2: 2, 3: 10, 4: 40, 5: 150},
		1: {3: 15, 4: 50, 5: 150},
		2: {3: 15, 4: 50, 5: 150},
		3: {3: 15, 4: 50, 5: 150},
		4: {3: 40, 4: 100, 5: 400},
		5: {3: 40, 4: 100, 5: 400},
		6: {3: 60, 4: 200, 5: 800},
		7: {3: 100, 4: 1000, 5: 5000},
	}
	fruitsScatterPay = map[int]int{3: 5, 4: 20, 5: 100}
	fruitsWeights    = []int{40, 40, 40, 40, 25, 25, 15, 8}
)

const (
	fruitsWildWeight    = 8
	fruitsScatterWeight = 4
)

type fruitsWin struct{ line, count, amount int }

type fruitsState struct {
	grid                               []int
	lineBet, win, last                 int
	wins                               []fruitsWin
	scatters, gamble, gambleLeft, card int
	guessed                            bool
	spins, flips                       int
	history                            []int
}

func fruitsStart() *fruitsState {
	g := make([]int, fruitsReels*fruitsRows)
	for i := range g {
		g[i] = []int{7, 6, 0}[i%fruitsRows]
	}
	return &fruitsState{grid: g, lineBet: 1, wins: []fruitsWin{}, card: -1, history: []int{}}
}

func fruitsEncode(s *fruitsState) map[string]any {
	wins := make([][]int, len(s.wins))
	for i, w := range s.wins {
		wins[i] = []int{w.line, w.count, w.amount}
	}
	return map[string]any{
		"grid": s.grid, "lb": s.lineBet, "win": s.win, "last": s.last, "wins": wins, "sc": s.scatters,
		"g": s.gamble, "gl": s.gambleLeft, "card": s.card, "ok": s.guessed, "spins": s.spins, "flips": s.flips, "history": s.history,
	}
}

func fruitsDecode(m map[string]any) *fruitsState {
	ws := mustList(m["wins"])
	wins := make([]fruitsWin, len(ws))
	for i, w := range ws {
		l := mustInts(w)
		wins[i] = fruitsWin{l[0], l[1], l[2]}
	}
	return &fruitsState{
		grid: mustInts(m["grid"]), lineBet: mustInt(m["lb"]), win: mustInt(m["win"]), last: mustInt(m["last"]), wins: wins,
		scatters: mustInt(m["sc"]), gamble: mustInt(m["g"]), gambleLeft: mustInt(m["gl"]), card: mustInt(m["card"]), guessed: m["ok"] == true,
		spins: mustInt(m["spins"]), flips: mustInt(m["flips"]), history: mustInts(m["history"]),
	}
}

func fruitsValid(s *fruitsState) bool {
	if len(s.grid) != fruitsReels*fruitsRows || s.lineBet <= 0 || s.gamble < 0 || s.gambleLeft < 0 || s.gambleLeft > fruitsGambleSteps {
		return false
	}
	for _, x := range s.grid {
		if x < 0 || x >= fruitsSymbols {
			return false
		}
	}
	return true
}

func fruitsPick(reel int, rnd houseRand, scatter bool) int {
	w := slices.Clone(fruitsWeights)
	wild, sc := 0, 0
	if reel >= 1 && reel <= 3 {
		wild = fruitsWildWeight
	}
	if scatter {
		sc = fruitsScatterWeight
	}
	w = append(w, wild, sc)
	r := rnd.Intn(sumInts(w))
	for i, x := range w {
		if r < x {
			return i
		}
		r -= x
	}
	return 0
}

// fruitsRoll: a spin's symbols, reel by reel, top to bottom.
func fruitsRoll(rnd houseRand) []int {
	out := make([]int, 0, fruitsReels*fruitsRows)
	for reel := 0; reel < fruitsReels; reel++ {
		if rnd.Float64() < fruitsStacked {
			x := fruitsPick(reel, rnd, false)
			for k := 0; k < fruitsRows; k++ {
				out = append(out, x)
			}
		} else {
			for k := 0; k < fruitsRows; k++ {
				out = append(out, fruitsPick(reel, rnd, true))
			}
		}
	}
	return out
}

// fruitsEvaluate: the lines grid wins for lineBet, and the scatters' count.
func fruitsEvaluate(grid []int, lineBet int) ([]fruitsWin, int) {
	wins := []fruitsWin{}
	for li, line := range fruitsLines {
		syms := make([]int, fruitsReels)
		for r := range syms {
			syms[r] = grid[r*fruitsRows+line[r]]
		}
		first := syms[0]
		if first == fruitsWild || first == fruitsScatter {
			continue
		}
		n := 1
		for n < fruitsReels && (syms[n] == first || syms[n] == fruitsWild) {
			n++
		}
		if k := fruitsPay[first][n]; k > 0 {
			wins = append(wins, fruitsWin{li, n, k * lineBet})
		}
	}
	sc := 0
	for _, x := range grid {
		if x == fruitsScatter {
			sc++
		}
	}
	return wins, sc
}

func fruitsPayout(grid []int, lineBet int) int {
	wins, sc := fruitsEvaluate(grid, lineBet)
	t := 0
	for _, w := range wins {
		t += w.amount
	}
	return t + fruitsScatterPay[sc]*lineBet*len(fruitsLines)
}

func fruitsMove(st map[string]any, p int, op string, m map[string]any, purse func(int) int, rnd houseRand) *houseOut {
	s := fruitsDecode(st)
	o := &houseOut{pay: map[int]int{}, over: []int{}, played: []int{}, stake: map[int]int{}, back: map[int]int{}}
	var d int
	switch op {
	case "spin":
		lineBet := intArg(m, "v")
		stake := lineBet * len(fruitsLines)
		if !slices.Contains(fruitsLineBets, lineBet) || stake > purse(p) {
			return nil
		}
		s.grid = fruitsRoll(rnd)
		s.lineBet = lineBet
		s.wins, s.scatters = fruitsEvaluate(s.grid, lineBet)
		// (a win over 1 000 000 Lei is paid up to that)
		s.win = min(fruitsPayout(s.grid, lineBet), fruitsWinCap)
		s.last = s.win - stake
		s.spins++
		s.gamble = 0
		if s.win >= 1 && s.win <= fruitsGambleMax/2 {
			s.gamble = s.win
		}
		s.gambleLeft = 0
		if s.gamble > 0 {
			s.gambleLeft = fruitsGambleSteps
		}
		s.card = -1
		s.guessed = false
		s.history = append([]int{s.last}, s.history...)
		if len(s.history) > 10 {
			s.history = s.history[:10]
		}
		d = s.last
		o.stake[p], o.back[p], o.settle = -stake, s.win, true
		o.played = []int{p}
		if d > 0 {
			o.over = []int{p}
		}
	case "gamble":
		g := s.gamble
		if g <= 0 || s.gambleLeft <= 0 || g > purse(p) {
			return nil
		}
		card := rnd.Intn(52)
		isRed := card/13 < 2
		s.card = card
		s.flips++
		s.gambleLeft--
		if len(s.history) == 0 {
			s.history = []int{0}
		}
		o.stake[p], o.settle = -g, true
		if isRed == (m["red"] == true) {
			s.guessed = true
			s.gamble = g * 2
			if s.gambleLeft == 0 || s.gamble > fruitsGambleMax/2 {
				s.gambleLeft = 0
			}
			s.history[0] += g
			d = g
			o.back[p] = 2 * g
		} else {
			s.guessed = false
			s.gamble = 0
			s.gambleLeft = 0
			s.history[0] -= g
			d = -g
			o.back[p] = 0
		}
	case "collect":
		if s.gamble <= 0 {
			return nil
		}
		s.gamble = 0
		s.gambleLeft = 0
	default:
		return nil
	}
	if d != 0 {
		o.pay[p] = d
	}
	o.state = jsonNorm(fruitsEncode(s))
	return o
}

// ====== Roulette: one zero; straight 35:1, dozens and columns 2:1, the even chances 1:1 ======

const rouMaxBets = 25_000

var rouRed = map[int]bool{1: true, 3: true, 5: true, 7: true, 9: true, 12: true, 14: true, 16: true, 18: true, 19: true, 21: true, 23: true, 25: true, 27: true, 30: true, 32: true, 34: true, 36: true}

type rouBet struct {
	typ           string
	value, amount int
}

type rouState struct {
	bets         [][]rouBet
	ready        []bool
	phase        string
	result       int
	history, pay []int
	round        int
	left         []bool
}

func rouStart(n int) *rouState {
	s := &rouState{bets: make([][]rouBet, n), ready: make([]bool, n), phase: "bet", result: -1, history: []int{}, pay: make([]int, n), round: 1, left: make([]bool, n)}
	for i := range s.bets {
		s.bets[i] = []rouBet{}
	}
	return s
}

func rouEncode(s *rouState) map[string]any {
	bets := make([][]any, len(s.bets))
	for i, b := range s.bets {
		bets[i] = make([]any, len(b))
		for k, x := range b {
			bets[i][k] = []any{x.typ, x.value, x.amount}
		}
	}
	return map[string]any{"bets": bets, "ready": s.ready, "phase": s.phase, "result": s.result, "history": s.history, "pay": s.pay, "round": s.round, "left": s.left}
}

func rouDecode(m map[string]any) *rouState {
	bl := mustList(m["bets"])
	bets := make([][]rouBet, len(bl))
	for i, b := range bl {
		xs := mustList(b)
		bets[i] = make([]rouBet, len(xs))
		for k, x := range xs {
			l := mustList(x)
			bets[i][k] = rouBet{mustString(l[0]), mustInt(l[1]), mustInt(l[2])}
		}
	}
	left := make([]bool, len(bl))
	if l, ok := m["left"].([]any); ok {
		left = mustBools(l)
	}
	return &rouState{bets: bets, ready: mustBools(m["ready"]), phase: mustString(m["phase"]), result: mustInt(m["result"]), history: mustInts(m["history"]),
		pay: mustInts(m["pay"]), round: mustInt(m["round"]), left: left}
}

// rouMultiplier: what a bet pays on top of the stake when n comes up (0: it loses).
func rouMultiplier(typ string, value, n int) int {
	switch typ {
	case "n":
		if n == value {
			return 35
		}
	case "red":
		if rouRed[n] {
			return 1
		}
	case "black":
		if n != 0 && !rouRed[n] {
			return 1
		}
	case "even":
		if n != 0 && n%2 == 0 {
			return 1
		}
	case "odd":
		if n%2 == 1 {
			return 1
		}
	case "low":
		if n >= 1 && n <= 18 {
			return 1
		}
	case "high":
		if n >= 19 && n <= 36 {
			return 1
		}
	case "dozen":
		if n != 0 && (n-1)/12+1 == value {
			return 2
		}
	case "column":
		if n != 0 && (n-1)%3+1 == value {
			return 2
		}
	}
	return 0
}

func rouValid(typ string, value int) bool {
	switch typ {
	case "n":
		return value >= 0 && value <= 36
	case "red", "black", "even", "odd", "low", "high":
		return true
	case "dozen", "column":
		return value >= 1 && value <= 3
	}
	return false
}

func rouSum(b []rouBet) int {
	t := 0
	for _, x := range b {
		t += x.amount
	}
	return t
}

func rouAll(l []bool) bool {
	for _, x := range l {
		if !x {
			return false
		}
	}
	return true
}

func rouAnyBets(s *rouState) bool {
	for _, b := range s.bets {
		if len(b) > 0 {
			return true
		}
	}
	return false
}

func rouSpin(s *rouState, rnd houseRand) {
	n := rnd.Intn(37)
	s.result = n
	s.history = append([]int{n}, s.history...)
	if len(s.history) > 14 {
		s.history = s.history[:14]
	}
	for i, b := range s.bets {
		t := 0
		for _, x := range b {
			if k := rouMultiplier(x.typ, x.value, n); k > 0 {
				t += x.amount * k
			} else {
				t -= x.amount
			}
		}
		s.pay[i] = t
	}
	s.phase = "spun"
}

// rouPlay is Roulette.move: "" when done, else why not.
func rouPlay(s *rouState, p int, op, typ string, value, amount, purse int, host bool, rnd houseRand) string {
	switch op {
	case "bet":
		if s.phase != "bet" || s.ready[p] {
			return "not now"
		}
		if !rouValid(typ, value) || amount < 1 {
			return "bet"
		}
		if amount > purse {
			return "purse"
		}
		if len(s.bets[p]) >= 40 {
			return "too many"
		}
		if rouSum(s.bets[p])+amount > rouMaxBets {
			return "too much"
		}
		s.bets[p] = append(s.bets[p], rouBet{typ, value, amount})
	case "undo":
		if s.phase != "bet" || s.ready[p] {
			return "not now"
		}
		if len(s.bets[p]) > 0 {
			s.bets[p] = s.bets[p][:len(s.bets[p])-1]
		}
	case "clear":
		if s.phase != "bet" || s.ready[p] {
			return "not now"
		}
		s.bets[p] = []rouBet{}
	case "back":
		if !s.left[p] {
			return "not away"
		}
		s.left[p] = false
		if s.phase == "bet" && len(s.bets[p]) == 0 {
			s.ready[p] = false
		}
	case "ready", "leave":
		if op == "leave" {
			s.left[p] = true
		}
		if s.phase != "bet" {
			if op == "leave" {
				return ""
			}
			return "not now"
		}
		s.ready[p] = true
		if rouAll(s.ready) && rouAnyBets(s) {
			rouSpin(s, rnd)
		} else if rouAll(s.ready) {
			copy(s.ready, s.left)
		}
	case "spin":
		if s.phase != "bet" || !host || !rouAnyBets(s) {
			return "not now"
		}
		rouSpin(s, rnd)
	case "next":
		if s.phase != "spun" {
			return "not now"
		}
		s.phase = "bet"
		s.round++
		for i := range s.bets {
			s.bets[i] = []rouBet{}
			s.ready[i] = s.left[i]
			s.pay[i] = 0
		}
	default:
		return "what?"
	}
	return ""
}

func rouMove(st map[string]any, p int, op string, m map[string]any, host bool, purse func(int) int, rnd houseRand) *houseOut {
	s := rouDecode(st)
	if p < 0 || p >= len(s.bets) {
		return nil
	}
	was := s.phase
	staked := make([]int, len(s.bets))
	for i, b := range s.bets {
		staked[i] = rouSum(b)
	}
	typ, _ := m["type"].(string)
	if rouPlay(s, p, op, typ, intArg(m, "value"), intArg(m, "v"), purse(p), host, rnd) != "" {
		return nil
	}
	spun := was == "bet" && s.phase == "spun"
	playing := seatsWhere(len(s.bets), func(i int) bool { return len(s.bets[i]) > 0 })
	o := &houseOut{state: jsonNorm(rouEncode(s)), pay: map[int]int{}, profit: map[int]int{}, played: playing, stake: map[int]int{}, back: map[int]int{}, settle: spun}
	if op != "next" {
		for i := range s.bets {
			if d := staked[i] - rouSum(s.bets[i]); d != 0 {
				o.pay[i] = d
				o.stake[i] = d
			}
		}
	}
	if spun {
		o.over = []int{}
		for _, i := range playing {
			back := s.pay[i] + rouSum(s.bets[i])
			o.back[i] = back
			if back != 0 {
				o.pay[i] += back
			}
			if s.pay[i] != 0 {
				o.profit[i] = s.pay[i]
			}
			if s.pay[i] > 0 {
				o.over = append(o.over, i)
			}
		}
	}
	return o
}

// ====== Blackjack: six decks; hit, stand, double; the dealer draws to 17. The shoe and the hole card stay here ======

const (
	bjDecks  = 6
	bjMaxBet = 100_000
)

type bjState struct {
	shoe, dealer []int
	hands        [][]int
	bets         []int
	ready, done  []bool
	phase        string
	turn         int
	result       []int
	outcome      []string
	round        int
	left         []bool
}

func bjNewShoe(rnd houseRand) []int {
	a := make([]int, 52*bjDecks)
	for i := range a {
		a[i] = i % 52
	}
	for i := len(a) - 1; i > 0; i-- {
		j := rnd.Intn(i + 1)
		a[i], a[j] = a[j], a[i]
	}
	return a
}

func bjStart(n int, rnd houseRand) *bjState {
	s := &bjState{shoe: bjNewShoe(rnd), dealer: []int{}, hands: make([][]int, n), bets: make([]int, n), ready: make([]bool, n), done: make([]bool, n),
		phase: "bet", turn: -1, result: make([]int, n), outcome: make([]string, n), round: 1, left: make([]bool, n)}
	for i := range s.hands {
		s.hands[i] = []int{}
	}
	return s
}

func bjRank(card int) int { return card%13 + 1 }

// bjValue: the best value of cards, and whether an ace still counts 11.
func bjValue(cards []int) (int, bool) {
	v, ace := 0, false
	for _, c := range cards {
		v += min(bjRank(c), 10)
		if bjRank(c) == 1 {
			ace = true
		}
	}
	if ace && v+10 <= 21 {
		return v + 10, true
	}
	return v, false
}

func bjBlackjack(cards []int) bool {
	v, _ := bjValue(cards)
	return len(cards) == 2 && v == 21
}

func bjEncode(s *bjState) map[string]any {
	return map[string]any{
		"shoe": s.shoe, "dealer": s.dealer, "hands": s.hands, "bets": s.bets, "ready": s.ready, "done": s.done,
		"phase": s.phase, "turn": s.turn, "result": s.result, "outcome": s.outcome, "round": s.round, "left": s.left,
	}
}

// bjPublic: what everyone sees: no shoe ("left": how many cards are in it), and the hole card only once it's played.
func bjPublic(s *bjState) map[string]any {
	m := bjEncode(s)
	m["shoe"] = []int{}
	m["left"] = len(s.shoe)
	if s.phase == "play" {
		d := []int{}
		if len(s.dealer) > 0 {
			d = append(d, s.dealer[0])
		}
		m["dealer"] = append(d, -1)
	}
	return m
}

func bjDecode(m map[string]any) *bjState {
	hl := mustList(m["hands"])
	hands := make([][]int, len(hl))
	for i, h := range hl {
		hands[i] = mustInts(h)
	}
	ol := mustList(m["outcome"])
	outcome := make([]string, len(ol))
	for i, x := range ol {
		outcome[i], _ = x.(string)
	}
	left := make([]bool, len(hl))
	if l, ok := m["left"].([]any); ok {
		left = mustBools(l)
	}
	return &bjState{shoe: mustInts(m["shoe"]), dealer: mustInts(m["dealer"]), hands: hands, bets: mustInts(m["bets"]), ready: mustBools(m["ready"]),
		done: mustBools(m["done"]), phase: mustString(m["phase"]), turn: mustInt(m["turn"]), result: mustInts(m["result"]), outcome: outcome,
		round: mustInt(m["round"]), left: left}
}

func bjDraw(s *bjState, rnd houseRand) int {
	if len(s.shoe) == 0 {
		s.shoe = bjNewShoe(rnd)
	}
	c := s.shoe[len(s.shoe)-1]
	s.shoe = s.shoe[:len(s.shoe)-1]
	return c
}

func bjAnyBet(s *bjState) bool {
	for _, b := range s.bets {
		if b > 0 {
			return true
		}
	}
	return false
}

func bjDeal(s *bjState, rnd houseRand) {
	if len(s.shoe) < 52 {
		s.shoe = bjNewShoe(rnd)
	}
	playing := seatsWhere(len(s.bets), func(i int) bool { return s.bets[i] > 0 })
	for k := 0; k < 2; k++ {
		for _, i := range playing {
			s.hands[i] = append(s.hands[i], bjDraw(s, rnd))
		}
		s.dealer = append(s.dealer, bjDraw(s, rnd))
	}
	for i := range s.hands {
		s.done[i] = !slices.Contains(playing, i) || bjBlackjack(s.hands[i])
	}
	s.phase = "play"
	s.turn = -1
	if bjBlackjack(s.dealer) {
		bjFinish(s)
		return
	}
	bjAdvance(s, rnd)
}

func bjAdvance(s *bjState, rnd houseRand) {
	next := -1
	for i := range s.hands {
		if i > s.turn && !s.done[i] {
			next = i
			break
		}
	}
	if next < 0 {
		for i := range s.hands {
			if !s.done[i] {
				next = i
				break
			}
		}
	}
	if next >= 0 {
		s.turn = next
		return
	}
	alive := false
	for i := range s.bets {
		if v, _ := bjValue(s.hands[i]); s.bets[i] > 0 && v <= 21 && !bjBlackjack(s.hands[i]) {
			alive = true
		}
	}
	if alive {
		for {
			if v, _ := bjValue(s.dealer); v >= 17 {
				break
			}
			s.dealer = append(s.dealer, bjDraw(s, rnd))
		}
	}
	bjFinish(s)
}

func bjFinish(s *bjState) {
	d, _ := bjValue(s.dealer)
	dealerBj := bjBlackjack(s.dealer)
	for i := range s.hands {
		bet := s.bets[i]
		if bet <= 0 {
			s.result[i], s.outcome[i] = 0, ""
			continue
		}
		h := s.hands[i]
		v, _ := bjValue(h)
		var r int
		var text string
		switch {
		case v > 21:
			r, text = -bet, "Ai depășit 21"
		case bjBlackjack(h) && !dealerBj:
			r, text = bet*3/2, "Blackjack!"
		case dealerBj && !bjBlackjack(h):
			r, text = -bet, "Dealerul are blackjack"
		case bjBlackjack(h):
			r, text = 0, "Egalitate"
		case d > 21:
			r, text = bet, "Dealerul a depășit"
		case v > d:
			r, text = bet, "Câștigat"
		case v == d:
			r, text = 0, "Egalitate"
		default:
			r, text = -bet, "Pierdut"
		}
		s.result[i], s.outcome[i] = r, text
	}
	s.phase = "done"
	s.turn = -1
}

// bjPlay is Blackjack.move: "" when done, else why not.
func bjPlay(s *bjState, p int, op string, amount int, purse func(int) int, host bool, rnd houseRand) string {
	allReady := func() bool { return rouAll(s.ready) }
	switch op {
	case "back":
		if !s.left[p] {
			return "not away"
		}
		s.left[p] = false
		if s.phase == "bet" && s.bets[p] == 0 {
			s.ready[p] = false
		}
	case "leave":
		s.left[p] = true
		switch s.phase {
		case "bet":
			s.bets[p] = 0
			s.ready[p] = true
			if allReady() && bjAnyBet(s) {
				bjDeal(s, rnd)
			}
		case "play":
			if !s.done[p] {
				s.done[p] = true
				if s.turn == p {
					bjAdvance(s, rnd)
				}
			}
		}
	case "bet", "skip":
		if s.phase != "bet" {
			return "not betting now"
		}
		if op == "bet" {
			if amount < 1 || amount > purse(p)+s.bets[p] || amount > bjMaxBet {
				return fmt.Sprintf("bet %d", amount)
			}
			s.bets[p] = amount
		} else {
			s.bets[p] = 0
		}
		s.ready[p] = true
		if allReady() && bjAnyBet(s) {
			bjDeal(s, rnd)
		} else if allReady() {
			copy(s.ready, s.left)
		}
	case "deal":
		if s.phase != "bet" || !host {
			return "not now"
		}
		if !bjAnyBet(s) {
			return "nobody bet"
		}
		for i := range s.bets {
			if !s.ready[i] {
				s.bets[i] = 0
			}
		}
		bjDeal(s, rnd)
	case "hit", "stand", "double":
		if s.phase != "play" || s.turn != p {
			return "not your turn"
		}
		h := s.hands[p]
		switch op {
		case "hit":
			h = append(h, bjDraw(s, rnd))
			s.hands[p] = h
			if v, _ := bjValue(h); v >= 21 {
				s.done[p] = true
			}
		case "stand":
			s.done[p] = true
		case "double":
			if len(h) != 2 || purse(p) < s.bets[p] {
				return "can't double"
			}
			s.bets[p] *= 2
			s.hands[p] = append(h, bjDraw(s, rnd))
			s.done[p] = true
		}
		bjAdvance(s, rnd)
	case "next":
		if s.phase != "done" {
			return "not over"
		}
		s.phase = "bet"
		s.round++
		s.turn = -1
		s.dealer = []int{}
		for i := range s.hands {
			s.hands[i] = []int{}
			s.bets[i] = 0
			s.ready[i] = s.left[i]
			s.done[i] = false
			s.result[i] = 0
			s.outcome[i] = ""
		}
	default:
		return "what?"
	}
	return ""
}

func bjMove(sec map[string]any, p int, op string, m map[string]any, host bool, purse func(int) int, rnd houseRand) *houseOut {
	s := bjDecode(sec)
	if p < 0 || p >= len(s.hands) {
		return nil
	}
	was := s.phase
	staked := slices.Clone(s.bets)
	if bjPlay(s, p, op, intArg(m, "v"), purse, host, rnd) != "" {
		return nil
	}
	ended := was != "done" && s.phase == "done"
	playing := seatsWhere(len(s.bets), func(i int) bool { return s.bets[i] > 0 })
	o := &houseOut{state: jsonNorm(bjPublic(s)), secret: jsonNorm(bjEncode(s)), pay: map[int]int{}, profit: map[int]int{}, played: playing,
		stake: map[int]int{}, back: map[int]int{}, settle: ended}
	if op != "next" {
		for i := range s.bets {
			if d := staked[i] - s.bets[i]; d != 0 {
				o.pay[i] = d
				o.stake[i] = d
			}
		}
	}
	if ended {
		o.over = []int{}
		for _, i := range playing {
			back := s.bets[i] + s.result[i]
			o.back[i] = back
			if back != 0 {
				o.pay[i] += back
			}
			if s.result[i] != 0 {
				o.profit[i] = s.result[i]
			}
			if s.result[i] > 0 {
				o.over = append(o.over, i)
			}
		}
	}
	return o
}
