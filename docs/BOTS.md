# Writing a bot for a Spectre Voice server

A bot is a program that connects to a Spectre Voice server the way the apps do: one WebSocket, JSON messages,
an identity that is a P-256 key pair the bot makes and keeps for itself. A community manager registers the key's
**uid** once; from then on every connection with that key is a bot of that community. There is no API key, no
account and nothing to sign up for anywhere: only the server you talk to knows the bot.

A complete, runnable example is in [`bots/echo`](bots/echo/main.go): it answers `!ping` with `pong` and greets
whoever comes into its voice channel. Start there; the rest of this page is the reference.

```sh
cd docs/bots/echo
go run . -uid                        # makes bot.key and prints the uid to register
go run . -server wss://voice.example.com/v1/ws
```

## What a bot is (and isn't)

- It is **one community's**: the one whose manager registered it. It can't enter, join, create or leave
  communities. A hello that asks for another community is refused (`why: "bot"`).
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
  (claim, slots, admin link, server bans) or the community's invites and join requests.
- A server has at most 10 bots per community and 64 bot connections at once.

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
 "name":"ignored for bots","client":"bot/echo-1.0","dev":"<a fixed random id>","password":"<server password, if any>"}
```

- `v: 2` says the signature names the host. (v1, without the host, is still accepted from old apps; don't use it.)
- `client`: start it with `bot/` and name your program and its version; it's shown to admins.
- `dev`: the same string every time this instance connects. A reconnection with the same `dev` replaces the
  old, half-dead connection at once instead of counting as a second one.
- `password`: only when the server has a join password (`SV_JOIN_PASSWORD`).
- `comm`: leave it out, or give the bot's own community id.
- `ch`: the voice channel it was in before a reconnect (optional): it lands back there if it could join it now
  without a password, else in the default one.

4. The server answers with the welcome, or `{"t":"error","op":"hello","why":…,"message":…}` and closes.

The **uid** is `base64url(SHA-256(SubjectPublicKeyInfo DER)[:15])`, 20 characters. The welcome tells it too; so
does `go run . -uid` in the example. Give it to a community manager (see *Registering* below).

### The welcome

```json
{"t":"welcome","sid":7,"uid":"…","token":"…","bot":true,
 "server":{"name":"…","motd":"…","version":"…","music":true,"files":true,…},
 "community":{"id":2,"slug":"club","name":"Club",…},"communities":[…],
 "channels":[{"id":4,"comm":2,"parent":0,"name":"general","kind":"text",…},{"id":5,"name":"Lobby","default":true,…}],
 "members":[{"sid":1,"uid":"…","name":"Ana","ch":5,"role":"owner"},{"sid":7,"uid":"<the bot>","name":"Echo","ch":5,"bot":true}],
 "roles":[…],"perms":["kick","move"],"role":"","space":"c2","staff":false,"live":[]}
```

- `bot: true` is only there for a registered bot. Without it your key isn't registered (yet): tell the manager
  the uid and reconnect.
- `token` opens the side connections (`/bus`, `/cf/…` channel files) as this identity; it lasts 24 hours and
  every new welcome brings a fresh one.
- `channels`: `kind` is `""` for a voice channel (with its own chat), `"text"` for a chat-only channel,
  `"spacer"` for a line. `default: true` is where newcomers land. `locked: true` has a password.
- `members`: everyone connected in this community now, the bot included. `ch` is their channel.
- `perms`: the names of the bot's permissions.

The bot starts in the community's default voice channel like anyone (it's "in" it for the chat and the member
list; nobody hears anything from it unless it opens voice).

## Messages the server sends

Every message is a JSON object with `"t"`. Unknown types should be ignored (new ones appear).

| `t` | Fields | When |
|---|---|---|
| `member` | `m: {sid,uid,name,ch,role,roles,client,mic,snd,away,talk,awayMsg,rtt,loss,since,vmuted,talker,bot}` | someone came, moved channel, or changed state; also one for the bot itself |
| `member-left` | `sid, uid` | a connection left the community |
| `msg` | `m: {id,to,from,name,text,at,edit?,del?}`, `n?` | a chat message the bot may read: in a channel of its community (`to: "c:<ch>"`), the community chat (`"g:<comm>"`) or a private message to it (`"d:<uid>:<uid>"`). An edit or a deletion comes as `msg` again with the same `id` (`edit` set, or `del: true` and an empty text). The music bot's notes come as `msg` with a negative `id` and `from: "bot-music-<comm>"` |
| `history` | `to, items: [msg…], more, before` | the answer to `history` |
| `dms` | `items: [{with,name,last}]` | the answer to `dms` |
| `channel` / `channel-del` | `c: {channel}` / `id` | a channel saved or removed |
| `moved` | `ch, by, kick?, why?` | the bot was moved to another channel by someone |
| `poke` | `from, name, text` | someone poked the bot |
| `relay` | `to, from, sid, name, data` | an app-to-app message (games); passed on, never kept |
| `role` | `role, roles, perms, staff` | the bot's permissions changed (a manager edited its registration) |
| `roles` | `items` | the community's roles changed |
| `community-info` | `community` | the community's name, description, icon or openness changed |
| `community` | the welcome's community fields again | the bot was moved out of its community (banned: `why: "banned"`); its connection then closes |
| `live` | `on, s: {room,uid,title,since,comm}` | a screen stream started or ended |
| `talk` | `sid, on` | who is talking in voice |
| `music` | `sid, ch, now, queue, paused, volume` | the music bot's state (if the server has one) |
| `kicked` | `by, why, ban?` | the bot was kicked, banned or unregistered; the connection closes right after |
| `pong` | `n` | the answer to `ping` |
| `done` | `op, uid?` | a moderation message of the bot's was carried out |
| `error` | `op, why, message` | something the bot sent was refused; `op` is that message's `t` |
| `bot-added` / `bot-deleted` | `bot: {…}` / `uid` | a bot was registered or removed in the community (everyone in it is told) |

The server pings every 25 s at the WebSocket level; answer with pongs (every library does) or the connection
is dropped after 75 s of silence.

## Messages a bot sends

| Message | Needs | Effect |
|---|---|---|
| `{"t":"ping","n":1}` | | `pong` |
| `{"t":"join","ch":5,"password":"…"}` | | move into a voice channel of the community (text channels can't be joined; locked ones want their password, 5 tries a minute; full ones refuse) |
| `{"t":"say","to":"c:5","text":"…","n":"my-id"}` | not muted | a chat message: `to` is `c:<channel>` (a channel the bot may read: unlocked, or the one it's in), `g:<comm>` (the community chat) or `u:<uid>` (a private message to someone in the community). Up to 4000 characters. `n` (optional, ≤ 64 chars) comes back on the `msg` so you can match it. A text starting with `!` in a voice channel also goes to the music bot, if the server has one |
| `{"t":"history","to":"c:5","before":0}` | | the last 50 messages of a chat (`before`: an id, for older pages; `more` says if there are). `after: <id>` instead gives the ones newer than that id (catching up after a reconnect; `more`: there are newer still); `around: <id>` the 25 up to it and the 25 after (`more`: older exist, `moreAfter`: newer) |
| `{"t":"dms"}` | | the bot's private conversations |
| `{"t":"search","q":"word","to":"c:5","before":0}` | | the messages with `q` in them (2–64 characters, case-insensitive), newest first, 50 a page (`before`: the last page's oldest id). `to` is a chat it may read, or `*` for all of them (its community's channels and chat, its own private ones). Answer: `{"t":"search","q","to","items":[msg…],"more"}`. Two a second |
| `{"t":"edit","id":…,"text":"…"}`, `{"t":"unsay","id":…}` | own message (`unsay` of others': `delete_messages`) | change or take back a message |
| `{"t":"poke","uid":"…","text":"…"}` | | a poke to someone connected (≤ 200 chars) |
| `{"t":"state","away":true,"awayMsg":"…"}` | | the bot's own state flags (`mic`, `snd`, `away`, `awayMsg`), shown in member lists |
| `{"t":"move","sid":3,"ch":6}` | `move` | move a connection to another voice channel (people below the bot's rank only) |
| `{"t":"ch-kick","sid":3,"why":"…"}` | `kick` | out of their channel, to the default one |
| `{"t":"kick","sid":3,"why":"…"}` | `kick` | off the server connection |
| `{"t":"mute","uid":"…","minutes":10}` / `unmute` | `mute_chat` | no writing in the community for a while (0 minutes: for ever) |
| `{"t":"ch-ban","uid":"…","ch":5,"minutes":60}` / `ch-unban` | `channel_ban` | keep someone out of a channel |
| `{"t":"ban","uid":"…","minutes":0,"why":"…"}` / `unban` | `ban` | out of the community |
| `{"t":"voice-mute","uid":"…","minutes":…}` / `voice-unmute` | `mute_voice` | nobody hears them |
| `{"t":"talker","sid":3,"on":true}` | `mute_voice` | let someone talk in a moderated channel |
| `{"t":"bans"}` | any of the above | the community's bans and mutes |
| `{"t":"ch-save",…}`, `{"t":"ch-del","id":…}` | `manage_channels` | create, edit, delete channels (fields: `id?`, `name`, `topic`, `parent`, `pos`, `max`, `default`, `password`, `kind`, `moderated`, `icon`, `pic`, `quality`) |
| `{"t":"music","cmd":"play","q":"…"}` | | the music bot (`play`, `skip`, `pause`, `resume`, `stop`, `vol`, `queue`); `music` permission steers it for everyone |
| `{"t":"cf-list","ch":5}` and the other `cf-*` | (`files` for others') | channel files |
| `{"t":"relay","to":"c:5","data":{…}}` | | an app-to-app message to a chat's readers (≤ 32 KB, 64 KB/s) |
| `{"t":"rtc",…}`, `{"t":"ice",…}` | `speak` | voice |

Moderation on a person needs the bot to outrank them (see above); otherwise `error why:"role"
message:"they rank as high as you"`.

## The bus: the bot's own records

`wss://<host>/bus?token=<the welcome's token>&space=<the welcome's "space">` is the community's shared state
(games, profiles, sounds…). On connect it sends `{"t":"state","state":{kind:{id:value}},…}`; after that every
change comes as `put` / `del`. A bot gets one kind of its own, **`bot:<slug>`** — the slug is its name in lower
case with everything but letters and digits turned into `-` (`"Echo Bot"` → `bot:echo-bot`; the `bots` reply
and `bot-added` tell it as `kind`):

```json
{"t":"put","kind":"bot:echo-bot","id":"counter","v":{"n":12}}
{"t":"del","kind":"bot:echo-bot","id":"counter"}
```

- Only the bot (and the community's managers) may write or delete records of that kind; everyone in the
  community reads them (they're in the `state` snapshot and the `put`s). Keep nothing private there.
- Up to 500 records of 16 KB each (ids: letters, digits and `_.:+/=~-`, ≤ 128 chars), kept on the server's disk.
- The server stamps every record with `_by` (the writer's uid); you can't set it.
- The other kinds follow their own rules (`profiles` and `intros`: only one's own id; `lei`, `scores`…: see
  `bus.go`). A bot is an ordinary member there.
- Anything else sent on the bus is relayed to the others in the space, with `from` and `relay: true`.

## Registering a bot (what the manager does)

A member with `manage_community` sends, on their own connection, in the community:

| Message | Answer |
|---|---|
| `{"t":"bot-add","uid":"<the bot's uid>","name":"Echo Bot","perms":["move","kick"]}` | `{"t":"bot-added","bot":{"uid","name","perms","kind","by","created"}}` to the whole community. Sending it again for the same uid changes its name and permissions |
| `{"t":"bot-del","uid":"…"}` | `{"t":"bot-deleted","uid":"…"}` to the whole community; the bot's connections are closed |
| `{"t":"bots"}` | `{"t":"bots","items":[{"uid","name","perms","kind","by","created"}]}` |

`perms` takes the names used by roles: `move`, `kick`, `ban`, `mute_chat`, `channel_ban`, `delete_messages`,
`invite`, `stream`, `bypass_locks`, `mention_all`, `speak`, `mute_voice`, `files`, `music`, `manage_channels`.
What the manager doesn't hold is dropped silently; `administrator`, `manage_community` and `manage_roles`
always are. `[]` (or none) makes a bot that can only read, write in the chat and poke.

Refusals (`error op:"bot-add"`): `why:"role"` (not a manager), `"bad"` (uid or name), `"taken"` (the key is a
bot of another community, or it has been on this server as a person — a person's key is never turned into a
bot, which would lock them into one community), `"full"` (10 bots already).

Removing a bot, banning it or deleting the community ends it; its key is a stranger again.

## Errors

`{"t":"error","op":"<your message's t>","why":"<short code>","message":"<English sentence>"}`. The codes a bot
will meet: `bot` (not for bots, or no voice without `speak`), `role` (no permission, or they rank as high),
`rate` (slow down), `bad` (a field is wrong, an empty or too long text, no such recipient), `gone` (no such
channel, message or person), `password` / `full` / `banned` (a channel), `muted`, `server` (the server couldn't;
try later). On `hello`: `auth` (the signature doesn't fit this host), `password`, `banned`, `many` (three
connections already), `full`, `bot` (another community asked), `name`.

## Good manners

- Keep one connection, reconnect with backoff, send the same `dev`.
- Answer in the chat you were spoken in (`msg.m.to`), and ignore your own messages (`from` is your uid).
- Don't keep what you read beyond what your feature needs; the people's data is theirs and stays on their
  server. Say in your bot's description what it stores.
- A bot has no business reading private messages between people: it only ever gets the ones sent to it.
