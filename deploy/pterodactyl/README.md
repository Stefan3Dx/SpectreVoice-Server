# Spectre Voice on Pterodactyl (for hosting partners)

Sell Spectre Voice servers with plans from **10 to 1000 places** (people connected at once), at your own prices.
Every plan is a code from the Spectre Voice partner API; the server redeems it by itself. You owe Spectre Voice the
share agreed in your partner contract (per place per 30 days, or a percent of the plan's price).

## 1. Import the egg

Admin › Nests › Import Egg › `egg-spectre-voice.json`. One allocation per server: the apps use its **TCP** port and
the voice uses the **same number in UDP** (both must be open on the node's firewall).

| Variable | Who sets it | What |
|---|---|---|
| `SV_MAX_USERS` | you (plan) | the plan's places, 10–1000 |
| `SV_PARTNER_CODE` | you (hidden) | the plan's code `SVP-…`, from the API below |
| `SV_PUBLIC_IP` | you | the node's public IP (or name): needed when the node is behind NAT |
| `SV_TURN`, `SV_TURN_USER`, `SV_TURN_PASS` | you, optional | a TURN relay for people on strict networks |
| `INSTALL_MUSIC` | you | `1`: yt-dlp + ffmpeg for the music bot (reinstall) |
| `SV_NAME`, `SV_PASSWORD`, `SV_CONTACT_EMAIL`, `SV_INVITE_BASE`, `SV_MUSIC` | the customer | name, password, GDPR contact, invite links, music bot |

Plans up to 30 places only need `SV_MAX_USERS` (30 places are free on every server) but still need a plan code, for
the accounting. Above 30, the code unlocks the extra places: at its first start the server redeems it, downloads the
official Plus build (signed, kept in `data/bin`) and restarts into it by itself. The Plus build checks in every 6
hours, so a changed or cancelled plan applies within 6 hours.

The customer owns their server: at the first start the console prints a claim code (also in `data/claim.code`); in
the app, *Server › Claim* makes them its owner. The customer is the data controller for their server (see the
operator guide); you are their host.

## 2. The partner API

Base `https://license.spectrevoice.com`, header `Authorization: Bearer svpk_…` (your key, from your partner contract).

```bash
K=svpk_...; API=https://license.spectrevoice.com/v1/partner
# A new plan (when a customer orders): its code goes into SV_PARTNER_CODE
curl -s -H "Authorization: Bearer $K" -d '{"slots":100,"ref":"<pterodactyl server uuid>","price_cents":499}' $API/subs
#  -> {"code":"SVP-XXXXXX-XXXXXX-XXXXXX-XXXXXX","slots":100}
# Upgrade / downgrade (also set SV_MAX_USERS to the same number)
curl -s -H "Authorization: Bearer $K" -d '{"code":"SVP-…","slots":250,"price_cents":999}' $API/subs/update
# Cancel (the customer stopped paying)
curl -s -H "Authorization: Bearer $K" -d '{"code":"SVP-…"}' $API/subs/cancel
# Your plans: code, places, your ref, the server it's on, last seen
curl -s -H "Authorization: Bearer $K" $API/subs
```

- `slots`: 10 to 1000. `ref`: your own reference (e.g. the Pterodactyl server's UUID or the order). `price_cents`:
  what the customer pays per month, only if your contract shares a percent of it.
- One code is one server. If a server is reinstalled (new data), it redeems the code again and the plan follows it.
- Automate it from your billing (WHMCS, Paymenter, Blesta…): on create → `subs`, on upgrade → `subs/update`, on
  suspend/terminate → `subs/cancel`, and put the code into the server's `SV_PARTNER_CODE` through the Pterodactyl
  application API (`PATCH /api/application/servers/{id}/startup`).

## 3. Billing

At the start of each month Spectre Voice sends you the report for the month before: for each plan, its places × days
active (changes counted from the day they were made) and what is due at your contract's terms. Your own
`GET /subs` lets you check it.

## Licence

The server is under the Elastic License 2.0 with a hosting permission (LICENSE.md). Hosting Spectre Voice servers
for others **as a business** needs a partner contract. Changing, disabling or getting around the places limit or the
plan checks is not permitted.
