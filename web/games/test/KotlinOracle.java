import io.github.stefan3dx.spectrevoice.core.Json;
import io.github.stefan3dx.spectrevoice.games.*;
import java.util.*;

// Records the apps' own Kotlin rules (the apps repo's compiled classes, as they are): games played with random but
// seeded moves, every outcome as the bus would carry it (Json.write). kotlin.mjs replays them through the web rules.
// Made again (after the apps' rules change: ./gradlew :desktop:compileKotlin in the apps repo) with:
//
//	CP=<apps>/desktop/build/classes/kotlin/main:<kotlin-stdlib.jar>
//	javac -cp $CP -d /tmp/o KotlinOracle.java && java -Dstdout.encoding=UTF-8 -cp $CP:/tmp/o KotlinOracle > /tmp/k.json
//	python3 pack.py /tmp/k.json kotlin-games.json && gzip -9 -n -f kotlin-games.json
public class KotlinOracle {
    static Random rnd = new Random(42);

    static Map<String, Object> out(Outcome o) {
        if (o == null) return null;
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("state", o.getState());
        m.put("secret", o.getSecret());
        Map<String, Object> pay = new LinkedHashMap<>();
        for (Map.Entry<Integer, Integer> e : o.getPay().entrySet()) pay.put(String.valueOf(e.getKey()), e.getValue());
        m.put("pay", pay);
        if (o.getProfit() != null) {
            Map<String, Object> pr = new LinkedHashMap<>();
            for (Map.Entry<Integer, Integer> e : o.getProfit().entrySet()) pr.put(String.valueOf(e.getKey()), e.getValue());
            m.put("profit", pr);
        } else m.put("profit", null);
        m.put("over", o.getOver());
        m.put("played", o.getPlayed());
        m.put("final", o.getFinal());
        return m;
    }

    static List<GameHub.Player> players(int n) {
        List<GameHub.Player> l = new ArrayList<>();
        for (int i = 0; i < n; i++) l.add(new GameHub.Player("u" + i, "P" + i));
        return l;
    }

    static Map<String, Object> mv(Object... kv) {
        Map<String, Object> m = new LinkedHashMap<>();
        for (int i = 0; i < kv.length; i += 2) m.put((String) kv[i], kv[i + 1]);
        return m;
    }

    interface Mover { Map<String, Object> pick(GameHub.Kind k, Map<String, Object> st, Map<String, Object> sec, int n); }

    static Map<String, Object> play(GameHub.Kind kind, int n, int[] purses, int steps, Mover mover) {
        List<GameHub.Player> ps = players(n);
        Ctx host = new Ctx(ps, true, i -> purses[i]);
        Ctx guest = new Ctx(ps, false, i -> purses[i]);
        Outcome o = Rules.INSTANCE.start(kind, host, null);
        Map<String, Object> st = o.getState(), sec = o.getSecret();
        List<Object> rec = new ArrayList<>();
        for (int k = 0; k < steps; k++) {
            int p = rnd.nextInt(n);
            List<Integer> w = Rules.INSTANCE.waitingOn(kind, st, sec);
            if (!w.isEmpty() && rnd.nextInt(4) != 0) p = w.get(0);
            Map<String, Object> m = mover.pick(kind, st, sec, n);
            boolean isHost = p == 0 && rnd.nextBoolean();
            Outcome r;
            try { r = Rules.INSTANCE.move(kind, st, sec, p, m, isHost ? host : guest); } catch (Exception e) { r = null; }
            rec.add(mv("p", p, "m", m, "host", isHost, "in", mv("state", st, "secret", sec), "out", out(r)));
            if (r != null) {
                Map<String, Object> next = new LinkedHashMap<>(r.getState());
                if (r.getFinal()) next.put("final", true);
                st = next;
                if (r.getSecret() != null) sec = r.getSecret();
                if (r.getFinal()) break;
            }
        }
        return mv("kind", kind.name(), "n", n, "purses", purses, "start", out(o), "steps", rec);
    }

    static final String[] PROMOS = {"q", "r", "b", "n", "x", "Q"};
    static final String[] RTYPES = {"n", "red", "black", "even", "odd", "low", "high", "dozen", "column", "nope"};

    public static void main(String[] a) {
        List<Object> games = new ArrayList<>();
        for (int g = 0; g < 4; g++) games.add(play(GameHub.Kind.TICTACTOE, 2, new int[]{1000, 1000}, 40, (k, st, sec, n) -> mv("cell", rnd.nextInt(11) - 1)));
        for (int g = 0; g < 4; g++) games.add(play(GameHub.Kind.CONNECT4, 2, new int[]{1000, 1000}, 120, (k, st, sec, n) -> mv("cell", rnd.nextInt(9) - 1)));
        for (int g = 0; g < 3; g++) games.add(play(GameHub.Kind.CHESS, 2, new int[]{1000, 1000}, 300, (k, st, sec, n) -> {
            Chess c = Chess.Companion.decode(st);
            List<int[]> legal = new ArrayList<>();
            for (int f = 0; f < 64; f++) for (int t : c.legal(f)) legal.add(new int[]{f, t});
            if (legal.isEmpty() || rnd.nextInt(8) == 0) return mv("from", rnd.nextInt(66) - 1, "to", rnd.nextInt(64));
            int[] x = legal.get(rnd.nextInt(legal.size()));
            return rnd.nextInt(5) == 0 ? mv("from", x[0], "to", x[1]) : mv("from", x[0], "to", x[1], "promo", PROMOS[rnd.nextInt(PROMOS.length)]);
        }));
        for (int g = 0; g < 4; g++) games.add(play(GameHub.Kind.BARBUT, 3, new int[]{1000, 20, 0}, 300, (k, st, sec, n) -> rnd.nextInt(12) == 0 ? mv("op", "stake", "v", rnd.nextInt(200) - 10) : mv("op", "roll")));
        for (int g = 0; g < 4; g++) games.add(play(GameHub.Kind.ROULETTE, 3, new int[]{1000, 60, 0}, 160, (k, st, sec, n) -> {
            // Mostly what a player would do in each phase, sometimes anything (refused the same way, or not)
            boolean betting = "bet".equals(st.get("phase"));
            int r = rnd.nextInt(20);
            if (r == 0) return mv("op", new String[]{"undo", "clear", "spin", "leave", "huh", "next"}[rnd.nextInt(6)]);
            if (r == 1) return mv("op", "bet", "type", RTYPES[rnd.nextInt(RTYPES.length)], "value", rnd.nextInt(40) - 1, "v", rnd.nextInt(80) - 5);
            if (!betting) return mv("op", "next");
            if (r < 12) {
                String ty = RTYPES[rnd.nextInt(RTYPES.length - 1)];
                int v = ty.equals("n") ? rnd.nextInt(37) : (ty.equals("dozen") || ty.equals("column")) ? 1 + rnd.nextInt(3) : 0;
                return mv("op", "bet", "type", ty, "value", v, "v", 1 + rnd.nextInt(40));
            }
            return mv("op", r < 18 ? "ready" : "spin");
        }));
        for (int g = 0; g < 1; g++) games.add(play(GameHub.Kind.SLOTS, 1, new int[]{500}, 200, (k, st, sec, n) -> rnd.nextInt(10) == 0 ? mv("op", "pull") : mv("op", "spin", "v", rnd.nextInt(700) - 5)));
        for (int g = 0; g < 5; g++) games.add(play(GameHub.Kind.BLACKJACK, 3, new int[]{1000, 50, 0}, 180, (k, st, sec, n) -> {
            Object ph = st.get("phase");
            int r = rnd.nextInt(16);
            if (r == 0) return mv("op", new String[]{"deal", "leave", "huh", "next", "double", "skip"}[rnd.nextInt(6)]);
            if ("bet".equals(ph)) return r < 10 ? mv("op", "bet", "v", rnd.nextInt(120) - 5) : mv("op", r < 13 ? "skip" : "deal");
            if ("play".equals(ph)) return mv("op", r < 6 ? "hit" : r < 11 ? "stand" : "double");
            return mv("op", "next");
        }));
        for (int g = 0; g < 4; g++) games.add(play(GameHub.Kind.BACKGAMMON, 2, new int[]{1000, 1000}, 600, (k, st, sec, n) -> {
            Backgammon.State b = Backgammon.INSTANCE.decode(st);
            int r = rnd.nextInt(20);
            if (r == 0) return mv("op", new String[]{"again", "roll", "huh"}[rnd.nextInt(3)]);
            if (r == 1) return mv("op", "move", "from", rnd.nextInt(27) - 2, "to", rnd.nextInt(27) - 3);
            if (b.getGameWinner() >= 0) return mv("op", "again");
            if (b.getDice().isEmpty()) return mv("op", "roll");
            var legal = Backgammon.INSTANCE.legal(b);
            if (legal.isEmpty()) return mv("op", "roll");
            var x = legal.get(rnd.nextInt(legal.size()));
            return mv("op", "move", "from", x.getFirst(), "to", x.getSecond());
        }));
        for (int g = 0; g < 4; g++) games.add(play(GameHub.Kind.POKER, 3, new int[]{1000, 300, 15}, 260, (k, st, sec, n) -> {
            Object ph = sec == null ? null : sec.get("phase");
            int r = rnd.nextInt(24);
            if (r == 0) return mv("op", new String[]{"blinds", "next", "leave", "huh", "raise"}[rnd.nextInt(5)], "v", rnd.nextInt(60) - 5);
            if ("done".equals(ph)) return r < 3 ? mv("op", "blinds", "v", 5 + rnd.nextInt(20)) : mv("op", "next");
            if (r < 6) return mv("op", "fold");
            if (r < 11) return mv("op", "check");
            if (r < 17) return mv("op", "call");
            if (r < 22) return mv("op", "raise", "v", rnd.nextInt(400));
            return mv("op", "allin");
        }));
        // Forfeits as the keeper sends them
        List<Object> forfeits = new ArrayList<>();
        for (GameHub.Kind k : new GameHub.Kind[]{GameHub.Kind.CHESS, GameHub.Kind.TICTACTOE, GameHub.Kind.CONNECT4, GameHub.Kind.BARBUT, GameHub.Kind.BACKGAMMON}) {
            Outcome s = Rules.INSTANCE.start(k, new Ctx(players(2), true, i -> 1000), null);
            for (int seat = 0; seat < 2; seat++) forfeits.add(mv("kind", k.name(), "seat", seat, "in", s.getState(), "out", out(Rules.INSTANCE.forfeit(k, s.getState(), seat, 2, "resign"))));
        }
        System.out.println(Json.INSTANCE.write(mv("games", games, "forfeits", forfeits)));
    }
}
