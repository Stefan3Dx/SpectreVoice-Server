import io.github.stefan3dx.spectrevoice.core.Json;
import io.github.stefan3dx.spectrevoice.games.*;
import java.util.*;

// The questions and the words of the apps' Trivia and Hangman (PartyData.kt), as JSON
public class PartyDump {
    public static void main(String[] a) {
        List<Object> trivia = new ArrayList<>();
        for (PartyData.Q q : PartyData.INSTANCE.getTRIVIA()) trivia.add(List.of(q.getQ(), q.getOptions(), q.getAnswer(), q.getCat()));
        List<Object> hang = new ArrayList<>();
        for (kotlin.Pair<String, String> p : PartyData.INSTANCE.getHANGMAN()) hang.add(List.of(p.getFirst(), p.getSecond()));
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("trivia", trivia); m.put("hangman", hang);
        System.out.println(Json.INSTANCE.write(m));
    }
}
