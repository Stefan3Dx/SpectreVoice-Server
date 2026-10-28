# Writing a bot for a Spectre Voice server

A bot is a program that connects to a Spectre Voice server the way the apps do: one WebSocket, JSON messages,
an identity that is a P-256 key pair the bot makes and keeps for itself. A server manager registers the key's
**uid** once; from then on every connection with that key is a bot of that server. There is no API key, no
account and nothing to sign up for anywhere: only the server you talk to knows the bot.

A complete, runnable example is in [`bots/echo`](bots/echo/main.go): it answers `!ping` with `pong` and greets
whoever comes into its voice channel. Start there; the rest of this page is the reference.

```sh
cd docs/bots/echo
go run . -uid                        # makes bot.key and prints the uid to register
go run . -server wss://voice.example.com/v1/ws
```

## What a bot is (and isn't)

- It is **the server's**: one server is one space, and the bot is in it like everyone else. It **never needs the
  server password**: the registration is what lets it in.
- It has **exactly the permissions it was given**, never more than the registering manager had at that time,
  and never `administrator`, `manage_community` or `manage_roles` (it can't change who may what, nor register
  other bots). `permsOf` ignores roles for a bot: the registration is the whole truth.
- For kicks, moves and bans it stands **above people with no role and below every role** (rank 1). It can move
  a plain member; it can't touch a moderator, an admin or the owner.
- It is **shown as a bot**: `"bot": true` in its welcome, and on its entry in every `members` list and `member`
  message, so the apps draw the robot icon they already use for the music bot. Its name is the one the manager
  gave it; the hello's `name` is ignored and the `name` message is refused.
- It **doesn't take a place**: it isn't counted against the server's places (people connected at once).
- It is **quieter**: 10 messages a second (bursts of 30) instead of 40/200; the usual byte budget for relays;
  three connections per key, like a person. Too fast gets `error why:"rate"`, and persistently too fast gets
  disconnected.
- Its words **wake no phones**: nothing it says sends a push notification (private messages or @mentions).
- It has **no voice unless allowed**: `rtc` and `ice` are refused without the `speak` permission. With it, the
  bot is a normal WebRTC peer (see the apps; it's beyond this page).
- It **can't stream**, can't whisper, can't register devices for push, can't use the server's own messages
  (claim, slots, admin link, server bans), the server's settings, its invites or its TV.
- A server has at most 10 registered bots and 64 bot connections at once.

Treat the bot's key like a password: whoever has it is the bot.

## Connecting

Endpoint: `wss://<host>/v1/ws` (plain `ws://` on a LAN, port 7880 by default).

1. The server sends `{"t":"challenge","n":"<base64 of 32 random bytes>","version":"…"}`.
2. The bot signs, with its P-256 key, SHA-256 of
   `"spectrevoice-auth-v2:" + host + "\n" + nonce` — `host` is the host name it dialed, lower case, without
   the port (`voice.example.com`, `192.168.1.5`, `::1` without brackets); `nonce` is the decoded bytes. The
   signature is ASN.1 DER (`ecdsa.SignASN1` in Go), base64.
3. The bot sends the hello within 15 seconds:

```json
{"t":"hello","v":2,"pub":"<base64 SubjectPublicKeyInfo DER>","sig":"<base64 signature>",
 "name":"ignored for bots","client":"bot/echo-1.0","dev":"<a fixed random id>"}
```

- `v: 2` says the signature names the host. (v1, without the host, is still accepted from old apps; don't use it.)
- `client`: start it with `bot/` and name your program and its version; it's shown to admins.
- `dev`: the same string every time this instance connects. A reconnection with the same `dev` replaces the
  old, half-dead connection at once instead of counting as a second one.
- `password`: not needed for a registered bot (people give the server password here, when there is one).
- `ch`: the voice channel it was in before a reconnect (optional): it lands back there if it could join it now
  without a password, else in the default one.

4. The server answers with the welcome, or `{"t":"error","op":"hello","why":…,"message":…}` and closes.

The **uid** is `base64url(SHA-256(SubjectPublicKeyInfo DER)[:15])`, 20 characters. The welcome tells it too; so
does `go run . -uid` in the example. Give it to a server manager (see *Registering* below).

### The welcome

```json
{"t":"welcome","sid":7,"uid":"…","token":"…","bot":true,
 "server":{"name":"…","about":"…","icon":"…","password":false,"motd":"…","version":"…","music":true,"files":true,…},
 "community":{"id":1,"name":"…",…},
 "channels":[{"id":4,"comm":1,"parent":0,"name":"general","kind":"text",…},{"id":5,"name":"Lobby","default":true,…}],
 "members":[{"sid":1,"uid":"…","name":"Ana","ch":5,"role":"owner"},{"sid":7,"uid":"<the bot>","name":"Echo","ch":5,"bot":true}],
 "roles":[…],"perms":["kick","move"],"role":"","space":"main","staff":false,"live":[]}
```

- `bot: true` is only there for a registered bot. Without it your key isn't registered (yet): tell the manager
  the uid and reconnect.
- `token` opens the side connections (`/bus`, `/cf/…` channel files) as this identity; it lasts 24 hours and
  every new welcome brings a fresh one.
- `channels`: `kind` is `""` for a voice channel (with its own chat), `"text"` for a chat-only channel,
  `"spacer"` for a line. `default: true` is where newcomers land. `locked: true` has a password.
- `server`: the server's name, description and icon as its admins set them; `password` says whether people need
  the server password. `community` is the same space in the older shape (id 1), kept for older programs.
- `members`: everyone connected to the server now, the bot included. `ch` is their channel.
- `perms`: the names of the bot's permissions.

The bot starts in the server's default voice channel like anyone (it's "in" it for the chat and the member
list; nobody hears anything from it unless it opens voice).

## Messages the server sends

Every message is a JSON object with `"t"`. Unknown types should be ignored (new ones appear).

| `t` | Fields | When |
|---|---|---|
| `member` | `m: {sid,uid,name,ch,role,roles,client,mic,snd,away,talk,awayMsg,rtt,loss,since,vmuted,talker,bot}` | someone came, moved channel, or changed state; also one for the bot itself |
| `member-left` | `sid, uid` | a connection left the server |
| `msg` | `m: {id,to,from,name,text,at,edit?,del?}`, `n?` | a chat message the bot may read: in a channel (`to: "c:<ch>"`), the server chat (`"g:1"`) or a private message to it (`"d:<uid>:<uid>"`). An edit or a deletion comes as `msg` again with the same `id` (`edit` set, or `del: true` and an empty text). The music bot's notes come as `msg` with a negative `id` and `from: "bot-music-1"` |
| `history` | `to, items: [msg…], more, before` | the answer to `history` |
| `dms` | `items: [{with,name,last}]` | the answer to `dms` |
| `channel` / `channel-del` | `c: {channel}` / `id` | a channel saved or removed |
| `moved` | `ch, by, kick?, why?` | the bot was moved to another channel by someone |
| `poke` | `from, name, text` | someone poked the bot |
| `relay` | `to, from, sid, name, data` | an app-to-app message (games); passed on, never kept |
| `role` | `role, roles, perms, staff` | the bot's permissions changed (a manager edited its registration) |
| `roles` | `items` | the server's roles changed |
| `server-info` | `server: {name,about,icon,password}` | an admin changed the server's name, description, icon or password |
| `live` | `on, s: {room,uid,title,since,comm,mode,sid}` | a screen stream started or ended (`mode` `"p2p"`: sent app to app, asked from session `sid`; bots can't take part) |
| `talk` | `sid, on` | who is talking in voice |
| `music` | `sid, ch, now, queue, paused, volume` | the music bot's state (if the server has one) |
| `kicked` | `by, why, ban?` | the bot was kicked, banned or unregistered; the connection closes right after |
| `pong` | `n` | the answer to `ping` |
| `done` | `op, uid?` | a moderation message of the bot's was carried out |
| `error` | `op, why, message` | something the bot sent was refused; `op` is that message's `t` |
| `bot-added` / `bot-deleted` | `bot: {…}` / `uid` | a bot was registered or removed (everyone on the server is told) |

The server pings every 25 s at the WebSocket level; answer with pongs (every library does) or the connection
is dropped after 75 s of silence.

## Messages a bot sends

| Message | Needs | Effect |
|---|---|---|
| `{"t":"ping","n":1}` | | `pong` |
| `{"t":"join","ch":5,"password":"…"}` | | move into a voice channel (text channels can't be joined; locked ones want their password, 5 tries a minute; full ones refuse) |
| `{"t":"say","to":"c:5","text":"…","n":"my-id"}` | not muted | a chat message: `to` is `c:<channel>` (a channel the bot may read: unlocked, or the one it's in), `g:1` (the server chat) or `u:<uid>` (a private message to someone on the server). Up to 4000 characters. `n` (optional, ≤ 64 chars) comes back on the `msg` so you can match it. A text starting with `!` in a voice channel also goes to the music bot, if the server has one |
| `{"t":"history","to":"c:5","before":0}` | | the last 50 messages of a chat (`before`: an id, for older pages; `more` says if there are). `after: <id>` instead gives the ones newer than that id (catching up after a reconnect; `more`: there are newer still); `around: <id>` the 25 up to it and the 25 after (`more`: older exist, `moreAfter`: newer) |
| `{"t":"dms"}` | | the bot's private conversations |
| `{"t":"search","q":"word","to":"c:5","before":0}` | | the messages with `q` in them (2–64 characters, case-insensitive), newest first, 50 a page (`before`: the last page's oldest id). `to` is a chat it may read, or `*` for all of them (the server's channels and chat, its own private ones). Answer: `{"t":"search","q","to","items":[msg…],"more"}`. Two a second |
| `{"t":"edit","id":…,"text":"…"}`, `{"t":"unsay","id":…}` | own message (`unsay` of others': `delete_messages`) | change or take back a message |
| `{"t":"poke","uid":"…","text":"…"}` | | a poke to someone connected (≤ 200 chars) |
| `{"t":"state","away":true,"awayMsg":"…"}` | | the bot's own state flags (`mic`, `snd`, `away`, `awayMsg`), shown in member lists |
| `{"t":"move","sid":3,"ch":6}` | `move` | move a connection to another voice channel (people below the bot's rank only) |
| `{"t":"ch-kick","sid":3,"why":"…"}` | `kick` | out of their channel, to the default one |
| `{"t":"kick","sid":3,"why":"…"}` | `kick` | off the server connection |
| `{"t":"mute","uid":"…","minutes":10}` / `unmute` | `mute_chat` | no writing on the server for a while (0 minutes: for ever) |
| `{"t":"ch-ban","uid":"…","ch":5,"minutes":60}` / `ch-unban` | `channel_ban` | keep someone out of a channel |
| `{"t":"ban","uid":"…","minutes":0,"why":"…"}` / `unban` | `ban` | off the server (its connections close; it can't come back until unbanned) |
| `{"t":"voice-mute","uid":"…","minutes":…}` / `voice-unmute` | `mute_voice` | nobody hears them |
| `{"t":"talker","sid":3,"on":true}` | `mute_voice` | let someone talk in a moderated channel |
| `{"t":"bans"}` | any of the above | the server's bans and mutes |
| `{"t":"ch-save",…}`, `{"t":"ch-del","id":…}` | `manage_channels` | create, edit, delete channels (fields: `id?`, `name`, `topic`, `parent`, `pos`, `max`, `default`, `password`, `kind`, `moderated`, `icon`, `pic`, `quality`) |
| `{"t":"music","cmd":"play","q":"…"}` | | the music bot (`play`, `skip`, `pause`, `resume`, `stop`, `vol`, `queue`); `music` permission steers it for everyone |
| `{"t":"cf-list","ch":5}` and the other `cf-*` | (`files` for others') | channel files |
| `{"t":"relay","to":"c:5","data":{…}}` | | an app-to-app message to a chat's readers (≤ 32 KB, 64 KB/s) |
| `{"t":"rtc",…}`, `{"t":"ice",…}` | `speak` | voice |

Moderation on a person needs the bot to outrank them (see above); otherwise `error why:"role"
message:"they rank as high as you"`.

## The bus: the bot's own records

`wss://<host>/bus?token=<the welcome's token>&space=<the welcome's "space">` is the server's shared state
(games, profiles, sounds…). On connect it sends `{"t":"state","state":{kind:{id:value}},…}`; after that every
change comes as `put` / `del`. A bot gets one kind of its own, **`bot:<slug>`** — the slug is its name in lower
case with everything but letters and digits turned into `-` (`"Echo Bot"` → `bot:echo-bot`; the `bots` reply
and `bot-added` tell it as `kind`):

```json
{"t":"put","kind":"bot:echo-bot","id":"counter","v":{"n":12}}
{"t":"del","kind":"bot:echo-bot","id":"counter"}
```

- Only the bot (and the server's managers) may write or delete records of that kind; everyone on the
  server reads them (they're in the `state` snapshot and the `put`s). Keep nothing private there.
- Up to 500 records of 16 KB each (ids: letters, digits and `_.:+/=~-`, ≤ 128 chars), kept on the server's disk.
- The server stamps every record with `_by` (the writer's uid); you can't set it.
- The other kinds follow their own rules (`profiles` and `intros`: only one's own id; `lei`, `scores`…: see
  `bus.go`). A bot is an ordinary member there.
- Anything else sent on the bus is relayed to the others in the space, with `from` and `relay: true`.

## Registering a bot (what the manager does)

A member with `manage_community` ("Manage the server" in the apps) sends, on their own connection:

| Message | Answer |
|---|---|
| `{"t":"bot-add","uid":"<the bot's uid>","name":"Echo Bot","perms":["move","kick"]}` | `{"t":"bot-added","bot":{"uid","name","perms","kind","by","created"}}` to everyone on the server. Sending it again for the same uid changes its name and permissions |
| `{"t":"bot-del","uid":"…"}` | `{"t":"bot-deleted","uid":"…"}` to everyone on the server; the bot's connections are closed |
| `{"t":"bots"}` | `{"t":"bots","items":[{"uid","name","perms","kind","by","created"}]}` |

`perms` takes the names used by roles: `move`, `kick`, `ban`, `mute_chat`, `channel_ban`, `delete_messages`,
`invite`, `stream`, `bypass_locks`, `mention_all`, `speak`, `mute_voice`, `files`, `music`, `manage_channels`.
What the manager doesn't hold is dropped silently; `administrator`, `manage_community` and `manage_roles`
always are. `[]` (or none) makes a bot that can only read, write in the chat and poke.

Refusals (`error op:"bot-add"`): `why:"role"` (not a manager), `"bad"` (uid or name), `"taken"` (the key is a
bot already, or it has been on this server as a person — a person's key is never turned into a bot), `"full"` (10 bots already).

Removing a bot or banning it ends it; its key is a stranger again.

## Errors

`{"t":"error","op":"<your message's t>","why":"<short code>","message":"<English sentence>"}`. The codes a bot
will meet: `bot` (not for bots, or no voice without `speak`), `role` (no permission, or they rank as high),
`rate` (slow down), `bad` (a field is wrong, an empty or too long text, no such recipient), `gone` (no such
channel, message or person), `password` / `full` / `banned` (a channel), `muted`, `server` (the server couldn't;
try later). On `hello`: `auth` (the signature doesn't fit this host), `password`, `banned`, `many` (three
connections already), `full`, `bot` (registered in a community this server no longer has: register it again), `name`.

## Good manners

- Keep one connection, reconnect with backoff, send the same `dev`.
- Answer in the chat you were spoken in (`msg.m.to`), and ignore your own messages (`from` is your uid).
- Don't keep what you read beyond what your feature needs; the people's data is theirs and stays on their
  server. Say in your bot's description what it stores.
- A bot has no business reading private messages between people: it only ever gets the ones sent to it.
