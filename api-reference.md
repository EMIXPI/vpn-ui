# vpn-ui panel API reference

Request and response shapes for the panel's HTTP API, covering the 14 VPN and relay
protocols this fork adds on top of upstream 3x-ui, plus the accounts / membership layer.

The source of truth for each protocol's settings shape is `web/service/protocoldefaults.go`
(the Go table) and `web/assets/js/model/inbound.js` (the browser model it was ported from).
If this document and those disagree, they are right.

---

## 1. Base URL, auth and the two things that surprise everyone

### Base path

Every route lives under the panel's configured base path, which is randomised at install
time and settable from Panel Settings:

```
https://HOST:PORT/<basePath>/panel/api/inbounds/list
```

`<basePath>` already carries its leading and trailing slashes internally; in a URL it is one
path segment, e.g. `/aX9k2m/`. A request to `/panel/api/...` without it does not 404 with a
useful message, it simply does not match a route.

### Auth: a session cookie, not a token

There is no API key. Log in first and keep the cookie:

```sh
curl -c jar.txt -X POST 'https://HOST:PORT/<basePath>/login' \
  -d 'username=admin' -d 'password=secret'
# with per-admin 2FA enabled, add: -d 'twoFactorCode=123456'
```

Then send `-b jar.txt` on every call.

**Unauthenticated API requests get `404`, not `401`.** `checkAPIAuth`
(`web/controller/api.go`) aborts with 404 to hide which endpoints exist. A 404 from
`/panel/api/...` therefore means "not logged in" at least as often as it means "wrong URL".

### Gotcha 1: bodies are form-urlencoded, not JSON

The panel's own frontend posts through axios with `Qs.stringify`, so **every POST body is
`application/x-www-form-urlencoded`**, and the Go side binds it with Gin's `ShouldBind` +
`form:` struct tags. A JSON body works only where a handler happens to bind both; do not
rely on it.

Anything structurally nested is passed as **a JSON string inside a form field**. For an
inbound that is `settings`, `streamSettings` and `sniffing`:

```sh
curl -b jar.txt -X POST 'https://HOST:PORT/<basePath>/panel/api/inbounds/add' \
  --data-urlencode 'remark=l2tp-main' \
  --data-urlencode 'enable=true' \
  --data-urlencode 'listen=' \
  --data-urlencode 'port=1701' \
  --data-urlencode 'protocol=l2tp' \
  --data-urlencode 'settings={"clients":[{"id":"alice","password":"s3cret","email":"alice@example.com","enable":true}]}' \
  --data-urlencode 'streamSettings={}' \
  --data-urlencode 'sniffing={}'
```

Repeated keys are how arrays arrive (`Qs` `arrayFormat: 'repeat'`), e.g.
`inboundIds=3&inboundIds=7`. An empty `inboundIds=` is the sentinel for "the group was
cleared", and means none ticked rather than id 0.

### Gotcha 2: a denial is HTTP 200 with `success:false`

Every handler answers through one envelope (`web/entity/entity.go`, `entity.Msg`):

```json
{ "success": true, "msg": "Inbound created successfully", "obj": { } }
```

A refusal, a validation failure, a permission denial and an ownership denial all come back
as **HTTP 200** with:

```json
{ "success": false, "msg": "somethingWentWrong (Invalid port (must be 1-65535): 70000)", "obj": null }
```

There is no 403. Client code that branches on the status code treats every rejection as a
success. **Assert on `body.success`**, and read `body.msg` for the reason. The only non-200
you will see from the API group is the 404 for an unauthenticated request.

`obj` is `null` for message-only replies, an object for creates and single reads, and an
array for list endpoints.

---

## 2. Endpoint index

All paths are relative to `/<basePath>/panel/api/inbounds`. `POST` unless noted.
The permission column is the bit `requirePerm` enforces; a super admin bypasses all of them,
and a reseller's mask is derived from their role rather than stored.

| Method | Path | Permission | Purpose |
|---|---|---|---|
| GET | `/list` | accessInbounds | Every inbound the caller can see, with `clientStats` |
| GET | `/get/:id` | accessInbounds | One inbound |
| GET | `/getClientTraffics/:email` | accessInbounds | One account's traffic row |
| GET | `/getClientTrafficsById/:id` | accessInbounds | Same, keyed by client identity |
| GET | `/resellerBalance` | accessInbounds | Caller's reseller balance (answers "not a reseller" for others) |
| POST | `/add` | createInbound | Create an inbound |
| POST | `/update/:id` | editInbound | Replace an inbound |
| POST | `/del/:id` | deleteInbound | Delete an inbound |
| POST | `/import` | createInbound | Create from an exported inbound object |
| POST | `/reorder` | editInbound | Display order only |
| POST | `/addClient` | createClient | Add an account (target inbound is a **body** field) |
| POST | `/updateClient/:clientId` | editClient | Edit an account |
| POST | `/:id/delClient/:clientId` | deleteClient | Delete by identity |
| POST | `/:id/delClientByEmail/:email` | deleteClient | Delete by email |
| POST | `/:id/copyClients` | createClient | Copy accounts to another inbound |
| POST | `/bulkPreview` | bulkOperation | Dry-run a bulk op |
| POST | `/bulkUpdateClients` | bulkOperation | Apply a bulk op |
| POST | `/:id/resetClientTraffic/:email` | editClient | Zero one account's counters |
| POST | `/resetAllClientTraffics/:id` | bulkOperation | Zero every account on an inbound |
| POST | `/resetAllTraffics` | bulkOperation | Zero every inbound's counters |
| POST | `/delDepletedClients/:id` | deleteClient | Drop accounts past quota/expiry |
| POST | `/updateClientTraffic/:email` | editClient | Set counters directly |
| POST | `/clientIps/:email` | accessInbounds | Source addresses seen for an account |
| POST | `/clearClientIps/:email` | editClient | Forget them |
| POST | `/onlines`, `/lastOnline` | accessInbounds | Liveness |

Protocol-specific, documented in section 8:

| Method | Path | Purpose |
|---|---|---|
| GET | `/:id/ovpn/:proto` | Download an `.ovpn` (`proto` = `udp` or `tcp`), raw file, not the envelope |
| GET | `/:id/wgc-configs?email=` | Render a wg-c account's per-device `.conf`s |
| GET | `/:id/awg-configs?email=` | Same for AmneziaWG |
| GET | `/:id/gre-configs?email=` | Render a GRE account's per-peer parameters |
| GET | `/:id/ssh-configs?email=` | Render an SSH account's endpoints and links |
| POST | `/generate-openvpn-certs`, `/:id/generate-openvpn-certs` | Mint a CA + server cert + tls-crypt key |
| POST | `/generate-ocserv-cert`, `/:id/generate-ocserv-cert` | Mint an OpenConnect server cert |
| POST | `/generate-sstp-cert`, `/:id/generate-sstp-cert` | Mint an SSTP server cert |
| POST | `/generate-ikev2-cert`, `/:id/generate-ikev2-cert` | Mint an IKEv2 CA + server cert |
| POST | `/check-ikev2-cert` | Inspect an IKEv2 cert, returns its key type and any warning |

The id-less cert variants exist so material can be generated for an inbound that has not
been saved yet; the `:id` variants also persist it onto the inbound.

---

## 3. The inbound object

Top-level form fields on `/add` and `/update/:id` (`model.Inbound`, `form:` tags):

| Field | Type | Notes |
|---|---|---|
| `id` | int | `/update/:id` only, and must match the path |
| `remark` | string | Display label |
| `enable` | bool | `true` / `false` as strings |
| `listen` | string | Empty = all interfaces |
| `port` | int | 1-65535. GRE ignores it and the server picks one |
| `protocol` | string | See the table in section 5 |
| `settings` | JSON string | Per-protocol, sections 6 and 7 |
| `streamSettings` | JSON string | Xray transport. `{}` for every VPN/relay protocol |
| `sniffing` | JSON string | `{}` is fine |
| `total` | int64 | Inbound-wide traffic cap in bytes, 0 = unlimited |
| `expiryTime` | int64 | Unix ms, 0 = never |
| `trafficReset` | string | `never` (default) / `hourly` / `daily` / `weekly` / `monthly`. A cron job zeroes matching inbounds; an unrecognised value simply never matches one |
| `trafficMultiplierEnable` | bool | Weight usage past a threshold |
| `trafficMultiplierAfter` | int64 | Threshold in bytes on up+down |
| `trafficMultiplier` | float | Weight past the threshold. Defaults to 1 |
| `speedLimitEnable` | bool | Per-account rate limit, not a shared pool |
| `speedLimitSeparate` | bool | false = `speedLimitDown` caps both directions |
| `speedLimitDown`, `speedLimitUp` | int | KB/s, 0 = unlimited |
| `speedLimitAfter` | int64 | Threshold in bytes, 0 = immediate |
| `ipLimit` | int | Default cap on distinct source addresses per account, 0 = none |
| `ipLimitStrategy` | string | `reject` (default) or `accept` (evict oldest) |

`tag` is derived server-side from listen and port; do not send it.
`sortOrder` has no form tag on purpose, so an update cannot reset it.

**`/update/:id` binds into a fresh struct and copies an allowlist onto the stored row
unconditionally.** Any column the body omits is written back as its zero value. Read the
inbound first and echo back what you are not changing, or the traffic-multiplier and
speed-limit settings are silently wiped. `/add` has no such trap.

`GET /list` and `GET /get/:id` return the same object plus `clientStats`, an array of
`{id, inboundId, enable, email, up, down, total, expiryTime, reset, lastOnline}`.

---

## 4. Creating an inbound with a minimal body

The server fills in every settings key the caller leaves out, from the same table the
panel's own Add form starts from, and then validates the result
(`NormalizeInboundSettings`, called at the top of `AddInbound`). So this is a complete,
working L2TP inbound:

```
protocol=l2tp&port=1701&settings={"clients":[{"id":"alice","password":"s3cret","email":"alice@example.com","enable":true}]}
```

and this is a complete WireGuard one, with the server minting every key:

```
protocol=wg-c&port=51820&settings={"clients":[{"id":"bob@example.com","email":"bob@example.com","enable":true}]}
```

Rules:

- Defaults **only add absent keys**. A key you send is stored exactly as you sent it,
  including a falsy one: `"userLimit": 0` and `"ipsecEnable": false` are choices, not
  omissions.
- A body that already carries the full shape is stored byte-identical. That is what keeps
  the panel's own requests unchanged.
- `ipRanges` is assigned by the server before the defaults run, so omitting it gets you an
  auto-allocated pool rather than an empty one.
- A GRE inbound's `port` is bookkeeping only and is re-picked server-side.
- **openvpn, sstp and ikev2 cannot be created from a minimal body.** `validateInboundConfig`
  requires a server certificate, and there is no server-side generator on the create path.
  Call the matching `/generate-*-cert` endpoint first and put the returned PEM into
  `settings` (see section 8).

---

## 5. Protocol constants

| `protocol` | Kind | Hands out a tunnel IP | Settings section |
|---|---|---|---|
| `l2tp` | PPP over L2TP/IPsec | yes | 6.1 |
| `pptp` | PPP over PPTP | yes | 6.2 |
| `openvpn` | OpenVPN | yes | 6.3 |
| `openconnect` | ocserv / AnyConnect | yes | 6.4 |
| `sstp` | accel-ppp MS-SSTP | yes | 6.5 |
| `ikev2` | strongSwan IKEv2/IPsec | yes | 6.6 |
| `wg-c` | kernel WireGuard | yes | 6.7 |
| `awg` | AmneziaWG | yes | 6.8 |
| `gre` | GRE (IP proto 47) | yes | 6.9 |
| `mtproto` | MTProto proxy (relay) | no | 6.10 |
| `ssh` | in-binary SSH gateway (relay) | no | 6.11 |
| `anytls` | Xray-native | no | 7.1 |
| `tuic` | Xray-native | no | 7.2 |
| `naive` | Xray-native | no | 7.3 |

Note `wg-c`, not `wgc`. The literal string is `"wg-c"` (`model.WGC`).

Shared vocabulary across the addressed protocols:

- `userLimit` - devices per account. `0` = no limit, else `1..64`. An **absent** key is not
  the same as `0`: absent means a legacy single-device inbound.
- `userLimitStrategy` - at the cap, `accept` (evict the oldest device) or `reject`.
  Anything else is rejected at save time rather than silently coerced.
- `ipRanges` - the address pool, as inclusive host ranges **not CIDRs**:
  `"10.1.0.2-10.1.0.254"`, with a `"10.1.0.2-254"` last-octet shorthand. Both ends must sit
  in one `/24`. Panel-managed for most protocols; posting `10.1.0.0/24` is rejected.
- `dns1` / `dns2` - literal IPs or empty. A hostname is rejected: these are written into a
  client config as nameserver addresses.
- `mtu` - `0` means "let the protocol or kernel choose", otherwise 576-9216.
- `clientToClient` - let this inbound's accounts reach each other.
- `crossInbound` - let them reach other inbounds' accounts.
- `externalProxy` - `[{"dest":"cdn.example.com","port":443,"remark":"eu"}]`. Rewrites the
  address in generated links and configs only; no daemon reads it.

---

## 6. VPN and relay protocol settings

Each table is the complete key set for that protocol. "Default" is what the server fills in
when you omit the key.

### 6.1 l2tp

| Key | Type | Default |
|---|---|---|
| `ipsecEnable` | bool | `true` |
| `ipsecPsk` | string | minted, 16 chars |
| `allowRaw` | bool | `false` |
| `clientToClient` | bool | `false` |
| `crossInbound` | bool | `false` |
| `ipRanges` | string[] | `[]` (auto-assigned) |
| `dns1` | string | `"8.8.8.8"` |
| `dns2` | string | `"8.8.4.4"` |
| `mtu` | int | `1400` |
| `userLimit` | int | `1` |
| `userLimitStrategy` | string | `"accept"` |
| `clients` | object[] | `[]` |
| `externalProxy` | object[] | `[]` |

`ipsecEnable: true` with an empty `ipsecPsk` is rejected: libreswan would get a conn with no
key and every client would fail at phase 1 with nothing surfacing in the panel.

Client entry: `id` (the PPP username), `password`, `email`, `enable`, `expiryTime`, `tgId`,
`subId`, `comment`, `totalGB`, `limitIp`, `reset`, `slot`, `created_at`, `updated_at`.

**Two or more l2tp inbounds share one daemon**, so one value of `ipsecPsk`, `dns1` and `mtu`
applies to all of them. `CheckSharedDaemonConflicts` rejects a second inbound that disagrees
on any of the three rather than accepting a value it would then silently ignore, which was
the old failure mode: clients got a profile that could not authenticate and nothing in the
UI explained why. ikev2 is checked the same way.

### 6.2 pptp

The l2tp table minus `ipsecEnable`, `ipsecPsk` and `allowRaw`: `clientToClient`,
`crossInbound`, `ipRanges`, `dns1` `"8.8.8.8"`, `dns2` `"8.8.4.4"`, `mtu` `1400`,
`userLimit` `1`, `userLimitStrategy` `"accept"`, `clients`, `externalProxy`.
Same client entry, same shared-daemon rule.

### 6.3 openvpn

| Key | Type | Default |
|---|---|---|
| `udpEnable` | bool | `true` |
| `tcpEnable` | bool | `true` |
| `tcpPort` | int | `1194` |
| `separatePorts` | bool | `false` (TCP and UDP share `port`) |
| `tlsUseFile` | bool | `false` |
| `caCertFile`, `serverCertFile`, `serverKeyFile`, `tlsCryptFile` | string | `""` |
| `dns1` / `dns2` | string | `"8.8.8.8"` / `"8.8.4.4"` |
| `mtu` | int | `1500` |
| `caCert`, `caKey`, `serverCert`, `serverKey`, `tlsCrypt` | string | `""` (**required**) |
| `cipherMode` | string | `"all"` (`old` / `new` / `all` / `custom`) |
| `ciphers` | string[] | the 8-entry `all` preset, see below |
| `clientToClient`, `crossInbound` | bool | `false` |
| `ipRanges` | string[] | `[]` |
| `userLimit` | int | `1` |
| `userLimitStrategy` | string | `"accept"` |
| `clients`, `externalProxy` | array | `[]` |

Default `ciphers`, in order (the order **is** the `data-ciphers` preference order):
`AES-256-GCM`, `AES-128-GCM`, `CHACHA20-POLY1305`, `AES-256-CBC`, `AES-192-CBC`,
`AES-128-CBC`, `BF-CBC`, `DES-EDE3-CBC`. An empty list is rejected: openvpn then refuses
every negotiation instead of falling back.

At least one of `udpEnable` / `tcpEnable` must be true, and `caCert` + `serverCert` must be
non-empty. Client entry is the l2tp one.

### 6.4 openconnect

| Key | Type | Default |
|---|---|---|
| `dns1` / `dns2` | string | `"8.8.8.8"` / `"8.8.4.4"` |
| `mtu` | int | `1420` |
| `tlsUseFile` | bool | `false` |
| `certificateFile`, `keyFile` | string | `""` (path mode) |
| `certificate`, `key` | string | `""` (inline PEM) |
| `caCert` | string | `""` |
| `clientToClient`, `crossInbound` | bool | `false` |
| `ipRanges` | string[] | `[]` |
| `userLimit` | int | `1` |
| `userLimitStrategy` | string | `"accept"` |
| `clients`, `externalProxy` | array | `[]` |

Either `tlsUseFile: true` with both paths set, or both inline PEM fields set. Client entry
is the l2tp one.

Note: two devices on one account behind a single NAT collapse into one session, because
ocserv sends no `NAS-Port` for the panel to tell them apart.

### 6.5 sstp

Key for key identical to openconnect, default `mtu` `1420`. Same cert requirement (accel-pppd's
sstp module refuses to start without one). Same client entry.

### 6.6 ikev2

The openconnect table plus:

| Key | Type | Default |
|---|---|---|
| `authMode` | string | `"eap-mschapv2"` (or `psk`, `eap-tls`) |
| `psk` | string | `""` |
| `serverAddr` | string | `""` (falls back to the detected host) |
| `nattPort` | int | `4500` |

- `authMode: "psk"` requires a non-empty `psk`, and is a **single-account** mode: the shared
  secret is the whole authentication.
- Every mode except `psk` requires a server certificate.
- `serverAddr` must match the certificate's SAN or clients reject the connection.
- Windows clients need MODP-1024; iOS silently rejects ECDSA server certs, so use RSA.

Client entry is the l2tp one.

### 6.7 wg-c

| Key | Type | Default |
|---|---|---|
| `dns1` / `dns2` | string | `"1.1.1.1"` / `"1.0.0.1"` |
| `mtu` | int | `1420` |
| `serverPrivKey`, `serverPubKey` | string | `""`, minted server-side |
| `pskEnable` | bool | `false` |
| `clientToClient`, `crossInbound` | bool | `false` |
| `ipRanges` | string[] | `[]` |
| `userLimit` | int | `1` |
| `userLimitStrategy` | string | `"accept"` |
| `clients`, `externalProxy` | array | `[]` |

Note the DNS pair is Cloudflare here, not the PPP family's Google pair.

Client entry (identity is the **email**; there is no username or password, the public key is
the credential):

```json
{
  "id": "bob@example.com",
  "email": "bob@example.com",
  "enable": true,
  "privKey": "", "pubKey": "", "psk": "",
  "devices": [ {"privKey": "", "pubKey": "", "psk": ""} ],
  "expiryTime": 0, "tgId": "", "subId": "", "comment": "",
  "totalGB": 0, "limitIp": 0, "reset": 0, "slot": 0
}
```

`id` must equal `email`. Leave the key fields empty and `ReconcileKeys` mints one keypair
**per device slot**, sized to `userLimit`: WireGuard tracks a single endpoint per public
key, so two devices sharing one keypair cannot both be online.

If you **do** send `devices`, they are preserved verbatim. This used to be dropped on the
add path, which made the server mint fresh keys for devices 2..K and silently invalidate
every config already handed out for them.

### 6.8 awg

The wg-c table plus the AmneziaWG 1.0 obfuscation block:

| Key | Type | Default |
|---|---|---|
| `jc` | int | `4` |
| `jmin` | int | `8` |
| `jmax` | int | `80` |
| `s1` | int | `77` |
| `s2` | int | `90` |
| `h1`, `h2`, `h3`, `h4` | string | `""`, minted server-side |

`jmin` must not exceed `jmax`, and none of the five may be negative. Client entry is wg-c's.

### 6.9 gre

| Key | Type | Default |
|---|---|---|
| `mtu` | int | `0` (kernel picks: 1476 raw, 1464 under FOU) |
| `ttl` | int | `64` (0, or 1-255) |
| `ipsecEnable` | bool | `false` |
| `ipsecPsk` | string | minted, 24 chars |
| `allowRaw` | bool | `true` |
| `fouEnable` | bool | `false` |
| `fouPort` | int | `15547` |
| `clientToClient`, `crossInbound` | bool | `false` |
| `ipRanges` | string[] | `[]` |
| `userLimit` | int | `1` |
| `userLimitStrategy` | string | `"accept"` (parity only, GRE enforces K structurally) |
| `clients` | object[] | `[]` |

`ipsecEnable` + `allowRaw` give three modes: raw only, IPsec only, or either. `fouEnable`
is separate on purpose: FOU is Linux/OpenWrt-only, so bundling it with IPsec would lock
MikroTik and Cisco peers out of encryption. `fouEnable: true` with `fouPort: 0` is rejected.

Client entry (identity is the email; GRE carries no credential at all):

```json
{
  "id": "site-a@example.com",
  "email": "site-a@example.com",
  "enable": true,
  "peers": [ {"peerIp": "203.0.113.9", "remark": "branch router"} ],
  "expiryTime": 0, "tgId": "", "subId": "", "comment": "",
  "totalGB": 0, "limitIp": 0, "reset": 0, "slot": 0
}
```

`peers` has one slot per `userLimit` device, and its **length is the slot count**. An empty
`peerIp` is a supported, deliberate choice, not an incomplete record: that peer is served by
the shared catch-all tunnel and its return path is learned from its first packets, which is
what makes a customer on a dynamic IP work.

Two caveats worth knowing before you automate GRE: speed limiting only shapes traffic that
traverses Xray, and GRE has no ports, so it cannot survive CGNAT (many consumer ISPs drop
IP protocol 47 outright).

### 6.10 mtproto

Inbound settings are just `{"clients": []}`. Everything else is **per account**, because the
proxy keys its policy off the authenticated secret rather than the socket, so one inbound can
serve accounts with entirely different modes and links.

Client entry:

```json
{
  "id": "carol@example.com",
  "email": "carol@example.com",
  "secret": "0123456789abcdef0123456789abcdef",
  "enable": true,
  "modeClassic": true, "modeSecure": true, "modeTls": true,
  "tlsDomain": "www.google.com",
  "adtagEnable": false, "adtag": "",
  "userLimit": 0,
  "externalProxy": [],
  "expiryTime": 0, "tgId": "", "subId": "", "comment": "",
  "totalGB": 0, "limitIp": 0, "reset": 0
}
```

`secret` is 32 hex characters; leave it blank and the server mints one. At least one mode
must stay enabled: an account with none is dropped from the generated config entirely,
because an empty mode list would otherwise read as "unrestricted". The client-facing secret
per mode is `secret` (classic), `"dd"+secret` (secure) and `"ee"+secret+hex(tlsDomain)`
(FakeTLS). No `slot`: MTProto hands out no address.

### 6.11 ssh

| Key | Type | Default |
|---|---|---|
| `userLimit` | int | `0` (no limit) |
| `userLimitStrategy` | string | `"accept"` |
| `externalProxy` | object[] | `[]` |
| `clients` | object[] | `[]` |
| `hostKey` | string | `""`, minted ed25519 PEM, never shown in the UI |

`userLimit` defaults to `0` here and not `1`, matching what the panel's Add form creates.

Client entry: `id` (a **real SSH login username**, not the email), `password`, `email`,
`enable`, `expiryTime`, `tgId`, `subId`, `comment`, `totalGB`, `limitIp`, `reset`,
`created_at`, `updated_at`. No `slot`.

---

## 7. Xray-native protocol settings

These three are terminated by the core itself. They take a real `streamSettings` (TLS lives
there), no address pool, and no `userLimit`.

### 7.1 anytls

| Key | Type | Default |
|---|---|---|
| `clients` | object[] | `[]` |
| `paddingScheme` | string[] | the 9-line upstream default, below |

```
stop=8
0=30-30
1=100-400
2=400-500,c,500-1000,c,500-1000,c,500-1000,c,500-1000
3=9-9,500-1000
4=500-1000
5=500-1000
6=500-1000
7=500-1000
```

The scheme is server-authoritative: it is handed to the client in the session's settings
frame, so changing it never requires reconfiguring a client. Send `"paddingScheme": []`
explicitly to mean "no padding at all"; omitting the key gets you the default above.

Client entry: `password` plus the shared base (`email`, `limitIp`, `totalGB`, `expiryTime`,
`enable`, `tgId`, `subId`, `comment`, `reset`, `created_at`, `updated_at`). Passwords must be
unique within an inbound; a collision is rejected.

### 7.2 tuic

| Key | Type | Default |
|---|---|---|
| `clients` | object[] | `[]` |
| `congestionControl` | string | `"cubic"` (or `bbr`, `new_reno`) |
| `authTimeout` | int | `3` seconds |
| `zeroRttHandshake` | bool | `false` |
| `heartbeat` | int | `10` seconds |
| `udpTimeout` | int | `60` seconds |

The three timeouts read `0` as "use the built-in default"; negative is rejected. An unknown
`congestionControl` is rejected here rather than silently falling back to cubic, because a
client that picks a different algorithm talks past the server's pacing instead of failing.

Client entry: `id` (a uuid, and the identity), `password`, plus the shared base. TUIC
presents both halves on every connection.

Note the account list must be under `clients`. Upstream TUIC configs spell it `users`, and
the core deliberately does **not** accept that alias: quota, expiry and disable enforcement
all key off `clients`, so a config using `users` would connect happily and unmetered.

### 7.3 naive

| Key | Type | Default |
|---|---|---|
| `clients` | object[] | `[]` |
| `network` | string | `"tcp"` |
| `masquerade` | object | `{"type":"404","file":"","url":"","string":""}` |

`network` is `tcp` (HTTP/2 over TLS), `udp` (HTTP/3 over QUIC) or `"tcp,udp"` (both on one
port). The core also accepts `h2`/`http2` and `h3`/`http3`/`quic` as spellings. **This field,
not `streamSettings.network`, decides which wires the listener owns**:
`NormalizeNaiveInboundStream` forces its transport onto the stream. An unrecognised spelling
is rejected, because the core would read it as "both" and open a listener you did not ask for.

`masquerade.type` is `404`, `file`, `proxy` or `string`, and each reads exactly one companion
field (`file`, `url`, `string` respectively), which must be non-empty for that type. All four
keys are kept so switching type does not lose what was typed under the other one.

Client entry: `password`, `username`, plus the shared base. `username` is the HTTP Basic
username; **empty means "use the email"**, which is what every naive account created before
the field existed authenticates with. It must not contain a colon and must be unique within
the inbound. The email stays the accounting identity either way.

---

## 8. Protocol-specific endpoints

### Certificate generation

```sh
curl -b jar.txt -X POST 'https://HOST:PORT/<basePath>/panel/api/inbounds/generate-ocserv-cert'
```

```json
{ "success": true, "obj": { "certificate": "-----BEGIN CERTIFICATE-----\n...", "key": "-----BEGIN PRIVATE KEY-----\n..." } }
```

- `generate-openvpn-certs` returns `caCert`, `caKey`, `serverCert`, `serverKey`, `tlsCrypt`.
- `generate-ocserv-cert` and `generate-sstp-cert` return `certificate`, `key`.
- `generate-ikev2-cert` returns `certificate`, `key`, `caCert`. It takes **no parameters**:
  the SAN is the panel's own detected server IP. If your clients dial a different name, set
  `settings.serverAddr` to it and supply a certificate whose SAN matches, because charon's
  cert will not.
- `check-ikev2-cert` binds a whole inbound body (the same fields as `/add`) and returns
  `{"keyType": "RSA", "warning": "..."}`. Use it before saving: iOS silently rejects an
  ECDSA server cert, so `keyType` is worth asserting on.

With `:id` the material is written onto the inbound in content mode (`tlsUseFile: false`) and
the daemon is reloaded. Without it, the PEM is returned for you to put into `settings`
yourself, which is how you create openvpn/sstp/ikev2 in one pass:

```sh
CERT=$(curl -s -b jar.txt -X POST '.../generate-ocserv-cert')
# take .obj.certificate and .obj.key, embed them in the settings JSON, then POST /add
```

### Client config rendering

`GET /:id/wgc-configs?email=bob@example.com`:

```json
{ "success": true, "obj": [
  { "deviceIndex": 0, "ip": "10.7.0.8/29", "remark": "", "publicKey": "...", "config": "[Interface]\n..." }
] }
```

One entry per device slot, times one per external-proxy endpoint. `awg-configs` has the same
shape. `ssh-configs` returns `{remark, host, port, singbox, plain, link}` per endpoint, where
`singbox` is a sing-box `ssh` outbound and `link` an `ssh://` share link.

`gre-configs` returns per-peer parameters rather than a config file, since the peer is a
router you configure yourself: `{peerIndex, remark, peerIp, dynamic, serverIp, innerIp,
innerMask, gatewayIp, mtu, mode, ipsecPsk, ipsecId}`. `mode` is `raw`, `ipsec` or
`ipsec-or-raw`.

`GET /:id/ovpn/udp` returns the `.ovpn` file itself with
`Content-Type: application/x-openvpn-profile`, **not** the JSON envelope.

---

## 9. Accounts and membership

An **account** is one sellable identity that can be served on several inbounds of different
protocols under one quota, one expiry and one subscription. It sits above the settings JSON
rather than replacing it: `settings.clients` is maintained as a projection of the account
onto each member inbound, which is what leaves RADIUS, the slot allocator, every daemon
config writer and `GetXrayConfig` working unchanged.

### Account fields (`accounts` table)

| Field | JSON | Notes |
|---|---|---|
| Email | `email` | The identity. Unique, matched case-insensitively after trimming |
| SubID | `subId` | Subscription key, indexed and validated |
| UUID | `uuid` | vmess / vless / tuic |
| VpnUsername | `vpnUsername` | l2tp, pptp, openvpn, openconnect, sstp, ikev2, ssh login |
| Password | `password` | trojan, shadowsocks, anytls, naive, tuic, and every credential VPN |
| Auth | `auth` | hysteria |
| Security | `security` | vmess |
| Secret | `secret` | mtproto |
| NaiveUser | `naiveUser` | naive Basic-auth username; empty = use Email |
| TotalGB | `totalGB` | Bytes despite the name |
| ExpiryTime | `expiryTime` | Unix ms |
| Enable, Reset, LimitIP, TgID, Comment | | One set per account, however many inbounds |

Credentials are stored **per field, not per inbound**: one uuid serves every vmess membership,
one password every trojan membership. The projection picks the field the member inbound's
protocol keys on.

### Membership (`account_inbounds`)

`{accountId, inboundId, slot, flow, createdAt}`, composite primary key.

- `slot` is per **membership**, never per account: one email on N pool inbounds legitimately
  holds N slots at N different addresses.
- `flow` is a per-membership vless override; empty means the protocol default.
- There is also an `extra` column, not exposed in JSON, holding the verbatim original client
  entry. The projection overlays onto it rather than rebuilding, which is what stops a write
  from destroying wg-c/awg `devices` or GRE `peers`.

### Setting memberships over the API

`/addClient` and `/updateClient/:clientId` accept repeated **`inboundIds`** form keys naming
every inbound the account should be served on:

```sh
curl -b jar.txt -X POST 'https://HOST:PORT/<basePath>/panel/api/inbounds/addClient' \
  --data-urlencode 'id=3' \
  --data-urlencode 'settings={"clients":[{"id":"dave","password":"pw","email":"dave@example.com","enable":true}]}' \
  --data-urlencode 'inboundIds=3' \
  --data-urlencode 'inboundIds=7'
```

- `id` is the **target** inbound and arrives in the body, not the path. It is always included
  in the membership set whether or not you repeat it in `inboundIds`.
- Omitting `inboundIds` entirely means "just the target", and is the legacy single-inbound
  path.
- Sending `inboundIds=` (one empty value) means the group was explicitly cleared.
- The caller must own **every** inbound named. An admin holding one inbound cannot provision
  a live account on another admin's by listing it here; the whole request is refused.
- Membership changes only remove the account from inbounds the caller can actually see, so
  not ticking an invisible inbound never unprovisions the account there.

The `settings.clients` array on these two endpoints must hold exactly **one** client for the
membership machinery to resolve the email.

### Client identity per protocol

`:clientId` on `/updateClient/:clientId` and `/:id/delClient/:clientId` is the account's
identity, and which field that is depends on the protocol:

| Identity field | Protocols |
|---|---|
| `password` | trojan, l2tp, pptp, openvpn, openconnect, sstp, ikev2, anytls, naive |
| `email` | shadowsocks |
| `auth` | hysteria, hysteria2 |
| `id` | vmess, vless, tuic (uuid); ssh (login name); wg-c, awg, gre, mtproto (the email) |

For the four email-identity protocols the stored entry carries `id` equal to `email`; posting
one without it results in a request to `/updateClient/undefined` and an "empty client ID"
error. `/:id/delClientByEmail/:email` sidesteps the whole question.

---

## 10. Bulk operations

`/bulkPreview` and `/bulkUpdateClients` both bind a **single form field named `data`** whose
value is the JSON request. Not the usual per-field form binding:

```sh
curl -b jar.txt -X POST 'https://HOST:PORT/<basePath>/panel/api/inbounds/bulkUpdateClients' \
  --data-urlencode 'data={"op":"addDays","days":30,"skipDisabled":true,"targets":[{"inboundId":3,"email":"alice@example.com"}]}'
```

| Field | Type | Notes |
|---|---|---|
| `op` | string | `addDays`, `subDays`, `addTraffic`, `subTraffic`, `enable`, `disable`, `delete`, `freeze`, `unfreeze` |
| `days` | int64 | For the day ops |
| `amountBytes` | int64 | For the traffic ops |
| `skipFirstUse` | bool | Skip accounts that have never connected |
| `skipUnlimited` | bool | Skip accounts with no quota |
| `skipDisabled` | bool | Skip disabled accounts |
| `targets` | array | `[{"inboundId": 3, "email": "..."}]` |

Response `obj` is `{"applied": N, "skipped": M}`. `/bulkPreview` computes the same counts
without writing. The batch is refused outright unless the caller owns every inbound named:
a partial apply would be worse than a refusal.

---

## 11. Validation errors

`AddInbound` fills defaults and then validates, so a create either stores a complete,
parseable settings blob or fails with a message naming the field. All arrive as HTTP 200 with
`success:false`.

| Message contains | Cause |
|---|---|
| `"mtu" must be a number` | A numeric field sent as a string, including `""`. The protocol's own Go struct cannot unmarshal it, and the daemon config writer that hits it has nowhere to report from |
| `"mtu" must be 0 (protocol default) or 576-9216` | Out of range |
| `"dns1" must be an IP address` | A hostname where a resolver address goes |
| `"ipRanges" entry ... is not an address range` | A CIDR, a backwards range, or one spanning two `/24`s |
| `"userLimit" must be 0 (no limit) or 1-64` | Out of range |
| `"userLimitStrategy" must be one of "accept", "reject"` | A typo. The resolver would otherwise absorb it as `accept` |
| `"ipsecPsk" is required when "ipsecEnable" is true` | l2tp or gre |
| `"psk" is required when "authMode" is "psk"` | ikev2 |
| `"fouPort" is required when "fouEnable" is true` | gre |
| `"ciphers" must list at least one cipher` | openvpn |
| `"congestionControl" must be one of ...` | tuic |
| `"network" has an unknown transport` | naive |
| `"masquerade.url" is required when "masquerade.type" is "proxy"` | naive, and the `file` / `string` equivalents |
| `"clients" must be an array` | An object where the account list goes. `GetClients` ignores its own unmarshal error, so this would otherwise be an inbound that listens and can authenticate nobody |
| `OpenVPN certificate is required` | Also `SSTP` and `IKEv2` variants |
| `Duplicate email: ...` | Emails are the panel's global account identity, unique across every inbound |
| `Port already exists` | Another inbound holds it |

A validation failure on `/add` writes nothing.

---

## 12. Known traps

- **Whole-inbound update does not sync `client_traffics`.** An expiry or quota set by
  `/update/:id` never auto-disables the account. Use the client endpoints for per-account
  changes.
- **A client posted without `"enable": true` is filtered out of the generated Xray config.**
  The port listens, nobody can authenticate, and nothing is logged.
- **`security=tls` with a blank `certificateFile` makes Xray refuse the entire config**, not
  just that inbound. `up=0, down=0` on an inbound that should have traffic usually means it
  never worked at all.
- **Country geo files are not bundled.** Selecting one writes `ext:geoip_IR.dat:ir` and Xray
  refuses the whole config.
- Installing a protocol's **core** installs the server, not the outbound client.
