# thefeed Messenger Protocol — Draft RFC

- **Status:** DRAFT / pre-release. Request for comments.
- **Wire version:** 1 (sealed-cell transport); message envelope v1; registration record v1.
- **Stability:** No backward-compatibility guarantees yet. The messenger has not
  shipped a stable release, so breaking changes are on the table — that is the
  point of this document. Please tear it apart.

This document describes the end-to-end encrypted "lite messenger" that
thefeed tunnels over DNS. It is written so people who have never read the code
can review the design. Wire details are given to the byte; where this document
and the code disagree, the code is the source of truth — please file that as a
bug against this document.

Domains shown as `chat.example.com` are placeholders.

---

## 1. Goals and non-goals

**Goals**

1. **Works where only DNS works.** Every operation is a DNS query/response on a
   small set of operator sub-domains. No TCP, no HTTP, no TLS SNI.
2. **End-to-end confidentiality.** The server stores and forwards ciphertext it
   cannot read. Only the two endpoints derive the message content key.
3. **Uniform, content-hiding queries.** Every query is one fixed-size,
   random-looking cell, and in-context op payloads are *sealed* (the operation,
   peer, and read metadata are encrypted). Against a resolver **without** the feed
   passphrase the cells are indistinguishable noise. A passphrase-**aware**
   resolver (explicitly modeled in §2) can recover the cell *framing* but not the
   sealed op contents — see the honest scoping in §2 and §8.
4. **Fail-closed.** A client refuses to chat with a server whose feed-signing
   key it has not pinned. There is no opportunistic / unauthenticated mode.
5. **Amnesia-safe client.** A client that loses all local state can recover its
   identity from a 32-byte seed and resynchronise sequence numbers from the
   server.

**Non-goals (today)**

- Hiding *metadata from the server*: the server necessarily learns the routing
  pair (sender address → recipient address), message sizes, and timing. See §13.
- Forward secrecy of message content (the pair content key is derived from
  long-term identity keys; a deliberate trade-off, see §15-2).
- Group chat, multi-device sync, voice/media. One-to-one text only.
- Hiding *that DNS is being used* from a network observer who can see the
  destination domain.

---

## 2. Threat model

| Adversary | Assumed power | What the protocol defends |
|-----------|---------------|---------------------------|
| Passive resolver **without** the passphrase | Sees every query name + response | Cells are uniform, random labels; learns no framing, op, peer, counter, or content — chat is indistinguishable from other obfuscated feed traffic |
| Passive resolver **with** the public passphrase | Also knows `query_key` | **Recovers the cell framing** (un-masks selector+counter) → can group cells by session, see the handshake top-bit and the cleartext `kind` byte (REGISTER vs AUTH), and traffic-analyze a multi-cell SEND vs a single-cell poll. In-context op **payloads stay sealed**, so the op/peer/read-metadata and content remain hidden. (So the masking is an obfuscation layer against a passphrase-ignorant censor, **not** a metadata shield against a passphrase holder.) |
| Malicious/compromised server | Stores & routes all ciphertext, can drop/delay/reorder, can lie | Cannot read content; cannot substitute a peer's key (address = hash of identity key); **cannot forge a ✓✓** (it carries a recipient-signed pair-MAC — §12) though it *can* withhold one; cannot impersonate (per-op seal + pinned signing key). A later `ek` compromise is bounded by rotation (§9) and never exposes content |
| Active forger on the wire | Can inject DNS answers | Sealed responses fail closed; a forged 4-byte op tag costs ~2³² guesses, each a DNS round-trip, against per-**account** limits that the multi-domain spread does not multiply; only yields E2E ciphertext. The unauthenticated session-lost sentinel (`0xE5`) is injectable but its re-handshake is client-rate-limited (§10) |
| Peer | A registered account you talk to | Standard E2E peer; cannot misattribute a message to a third party (content key binds src,dst,seq) |

Explicit residual exposure to the **server**: routing pair, sizes, timing,
social graph among accounts on that server. This is the accepted cost of
store-and-forward over a third-party relay (§15-5).

---

## 3. Terminology

- **Identity key** — ed25519 key pair derived from the user's seed. Names the user.
- **Address** — first 12 bytes of `SHA-256(identity_pub)`. 20 base32 chars when shared.
- **Encryption key (enc key)** — x25519 key pair derived from the seed. Used for E2E and for the client↔server shared key.
- **ek** — the *server's* x25519 key. Clients run the session handshake against it.
- **Registration record** — self-signed envelope binding `enc_pub` to an identity, published to a server.
- **ChatInfo** — the server's signed capability descriptor (limits, domains, `ek`).
- **Cell** — the single uniform 25-byte unit every chat query carries.
- **Session** — a per-connection symmetric key (`Ksession`) plus a server-assigned 3-byte selector.
- **Pair** — an ordered (sender, recipient) relationship; sequence numbers and delivery counters are per-pair-per-server.

---

## 4. Architecture overview

```
   client  ───DNS queries on chat.example.com───▶  server (relay + key store)
     │  E2E content key (server never has it)            │
     └───────────────── message ciphertext ─────────────┘
                         (store-and-forward)
```

- Chat rides the same DNS transport as the feed reader, but on dedicated chat
  sub-domains. The server dispatches by domain; chat names are never parsed as
  feed channel queries.
- **Trust root:** the client pins the server's **feed-signing public key** (ed25519,
  carried in the `thefeed://…?sk=` import URI). The `ChatInfo` descriptor —
  including the server's `ek` — is served on a metadata channel and authenticated
  by a signature (an "ExtraBlock") under that pinned key. So the key the client
  runs ECDH against (`ek`) is delivered in-band but **signed**; a MITM cannot
  swap it.
- The server keeps, per account: a bounded inbox of ciphertext envelopes, the
  account's published registration record, and per-pair sequence/quota state.
  Everything is TTL'd.

### 4.1 Multiple chat domains and one identity

A server may advertise several chat sub-domains in `ChatInfo.domains[]`. The
client spreads queries across them (rotating by cell index + retry attempt) for
resilience against per-domain blocking or rate limiting; every domain reaches the
same server and the same account state.

Because identity is a single key pair, the user has the **same address on every
server**. A conversation binds to the one server it was started on (sequence
numbers and delivery ticks are per-server, §11.3/§12). Switching a conversation's
send server is an explicit user action that first re-checks the peer is
registered there; a deliberate switch is never silently reverted by an in-flight
send.

---

## 5. Identity and addresses

```
seed (32 bytes, the recovery secret)
 ├─ HKDF-SHA256(info="thefeed-chat-identity-v1")   → ed25519 identity key
 └─ HKDF-SHA256(info="thefeed-chat-encryption-v1") → x25519  enc key

address = SHA-256(identity_pub)[:12]               (12 bytes → 20 base32 chars)
```

The address is a hash commitment to the identity key, so a server cannot present
a different identity/enc key for a given address without being detected (the
client recomputes `address == SHA-256(identity_pub)[:12]` on every key fetch).

---

## 6. Registration (publishing your key)

A client publishes a **registration record** so peers can fetch its `enc_pub`
and so the server can authenticate it.

```
RegisterEnvelope (133 bytes):
  ver(1)=1 ‖ identity_pub(32) ‖ enc_pub(32) ‖ timestamp(4, BE unix) ‖ ed25519_sig(64)
  sig = Sign(identity, "thefeed-chat-register-v1" ‖ body_before_sig)
```

- Self-signed by the identity key; newest timestamp wins on re-registration.
- A peer fetching this record verifies the signature **and** that
  `Address(identity_pub)` equals the address it asked for.
- Registration happens via the REGISTER handshake (§9), which both opens a
  session and stores the record.
- Registration is **per server** and **opt-in**: the client only publishes to
  servers the user explicitly enabled. An untrusted server never silently
  receives the user's identity.

---

## 7. Capability discovery — `ChatInfo`

Served on a dedicated metadata channel, block-split like feed titles, with a
trailing signature block verified against the pinned signing key. TLV-encoded
(unknown types skipped, so fields can be added):

```
ChatInfo:
  min_version, max_version
  enabled            (operator can configure chat domains but turn chat off)
  domains[]          (the chat sub-domains, for multi-domain spreading)
  ek_pub(32)         (server x25519 session key)
  limits             (see §14)
```

A client that cannot fetch a valid, signed `ChatInfo` does not chat (fail-closed).

---

## 8. Transport — the uniform cell

Every chat query is exactly one **cell**:

```
cell (25 bytes) = selector(3) ‖ counter(3) ‖ payload(19)
```

encoded as a single base32 DNS label (40 chars) under a chat domain, e.g.
`<40 chars>.chat.example.com`. (A "double" multi-label hex encoding exists for
resolvers that mangle long single labels.)

**Masking.** `selector ‖ counter` are XOR-masked with a keystream
`HMAC-SHA256(query_key, "thefeed-chat-cell-mask-v1" ‖ payload)[:6]`. The payload varies per cell (it is sealed
ciphertext or a random-looking ephemeral-key slab), so the mask — and therefore
the visible label — is different every time, with no constant per-session prefix
and no low-counter zero runs. A resolver without the public passphrase (which
gates `query_key`) sees uniform randomness. **Anyone holding `query_key`** — the
server, but also a passphrase-aware resolver — recovers `selector` and `counter`
(see §2): the masking hides framing from a passphrase-ignorant observer, not from
a passphrase holder. The *sealed payload* of an in-context op stays encrypted
regardless.

The cell has **two byte-indistinguishable uses**:

- **In-context** (an operation on a live session): `selector` = the server's
  3-byte session ref; `payload` = `SealChat(Ksession, selector, counter, op‖fields)`.
- **Handshake**: `selector` = a client-chosen *setup tag* (top bit set to mark
  it); `payload` = a 19-byte slab of the handshake stream.

Dispatch: the server looks `selector` up in its live-session table. Hit →
in-context. Miss → if the handshake bit is set (or `counter == 0`), treat as a
handshake cell; otherwise return the session-lost sentinel (§10).

### 8.1 The per-op seal

```
SealChat(Ksession, selector, counter, pt):          // variable-length pt
  enc = HKDF(Ksession, info="thefeed-chat-seal-enc-v1")
  mac = HKDF(Ksession, info="thefeed-chat-seal-mac-v1")
  nonce16 = selector(3) ‖ 0x00 ‖ counter(4) ‖ 0×8    (one AES block; unique per query)
  ct  = AES-256-CTR(enc, nonce16, pt)
  tag = HMAC-SHA256(mac, nonce16 ‖ ct)[:4]
  return ct ‖ tag                                     // len(pt)+4, no padding
```

`SealChat` itself is **variable-length** (it seals any plaintext, e.g. the
133-byte REGISTER bootstrap in §9). Uniform cells come from a thin wrapper used
for in-context ops only: the op plaintext is first zero-padded to a fixed **15
bytes** (`op(1) + fields(14)`) before sealing, so every in-context cell payload
is exactly 19 bytes. The handshake bootstrap is sealed with `SealChat` directly
on its variable-length plaintext and streamed across 19-byte slabs (§9) — so the
"fixed 15 bytes" applies to in-context cells, not to `SealChat` as such.

The 4-byte tag is deliberately short: forging it requires ~2³² *online*,
rate-limited DNS round-trips, and success only yields an E2E-encrypted payload
(rationale in §15-1).

### 8.2 Configurable cell size

> **Status:** implemented. The default (Standard) reproduces today's wire; the
> knob adds Compact (blend) and Wide (speed).

A chat query is bigger than a feed query (40 chars vs an observed 26–31) because
it carries the whole sealed request, not just a block index — a length
fingerprint a censor can read with no passphrase. The fix is to make the fixed
**15-byte** op budget a parameter **`B`** (default **15**), trading query
*length* against query *count*:

```
cell wire size = selector(3) + counter(3) + sealed(B) + tag(4) = 10 + B bytes
base32 label   = ceil((10 + B) * 8 / 5) chars
```

| `B` | chars | preset / role |
|----:|------:|---------------|
| 6 | 26 | **Compact** — low end of the feed cloud |
| 8 | 29 | Compact — center of a measured feed cloud |
| **15** | **40** | **Standard** — today's default |
| 21 | 48 | **Wide** — fewer queries, faster (a length spike) |

Three properties make this safe with **no negotiation**:

- **Self-describing.** `selector`/`counter` are fixed at the front and `tag` is
  the last 4 bytes, so the server reads `B = label_bytes − 10` off each cell.
  AES-CTR is length-agnostic and the mask HMAC already spans the whole payload,
  so the seal is unchanged.
- **Per-client, not end-to-end.** `B` only governs how *one* client chunks a
  message *to the server*; the message envelope is identical at any `B`, so two
  peers interoperate at different `B` (one uploads Wide, the other fetches
  Compact).
- **Version-scoped, client-chosen.** The valid range is a property of the
  protocol version — **v1: `B ∈ [6, 21]`** (floor = framing overhead, ceiling =
  DNS single-label limits), a constant both ends know. The **client** picks `B`
  within it from *its own* resolver health; the server has no say and advertises
  nothing — it parses any length by self-description, and the query-name size only
  affects the client→resolver path, never the server. The version is the
  capability signal: the `ChatInfo` min/max **version** range (pinned, bound into
  the handshake) already tells the client which range applies, and because chat is
  unreleased, v1 includes variable `B` from the start — there is no "old server"
  to fall back for.

**Op framing.** An op that fits in `B` is one cell (zero-padded to `B`, uniform
as today). An op larger than `B` — only the address-carrying control ops, and
only at small `B` — is fragmented with `OP_FRAG` (op 9):
`op(1) ‖ idx(1) ‖ total(1) ‖ chunk`, reassembled server-side (bounded, like the
upload/handshake reassembly) then re-dispatched. The body `DATA` chunk becomes
`B − 2`. **No wire field carries `B`:** every cell on a connection is the same
budget, so the server reads `B` from the cell length (minus jitter) and infers
the chunk size as `B − 2`; a fragmented `SEND_START` re-dispatches with the
*cell's* `B`, not the reassembled-op length. Content, addresses, identity,
crypto, and **responses** (they ride the larger TXT answer, not the query name)
are all unaffected by `B`.

**Setting.** One **global** user setting: three fixed presets — **Compact /
Standard / Wide** — over `B ∈ [6, 21]`, plus **Auto** (the **default**).

**Auto mode.** Per server, Auto scores each preset by the **queries it spends per
message and how many of them error** (the fetcher's success/failure delta across
the send) — *not* by success alone, and not by latency. Cost = queries +
weighted errors, with a penalty if the send failed; the cheapest mode is
exploited, epsilon-greedy exploration keeps every mode measured (Compact is tried
last and, on a weak path, ranked out so it can't flood), and an unused mode is
re-measured after a while. This is what makes Auto safe on weak resolvers, where
Compact's fragmentation multiplies queries: the score sees the cost and avoids
it. The UI shows each mode's recent queries/errors.

**Auto mode.** Per server (each network path scores independently), Auto measures
each preset by recent send success and prefers the best while never abandoning
the others. A send "succeeds" for its budget if the cells reached the server
(a normal result *or* any server-status reply — replay/quota/… mean the transport
worked); only a transport failure (unreachable/timeout) counts against it. Scores
are an **EWMA** (recent sends dominate, so a mode that starts losing — or recovers
— is re-scored within a handful of messages), selection is **epsilon-greedy** (a
bounded fraction explores so a poor mode is still sampled but not hammered), and a
mode unused for a while is re-measured first — which is also how stale results age
out. The UI shows each mode's live score.

**Blending via deterministic jitter.** A small `B` lands every chat query
*inside* the feed's measured length range; the always-on background poll (one
cell at any `B`) then stops being a length tell. To avoid a *fixed-length* spike
within that cloud, each cell is padded to a jittered length — but the pad is
**deterministic per cell**, derived from a keystream of `selector ‖ counter`
under `query_key`, **never fresh randomness per send**. This is load-bearing:
a cell is retransmitted byte-for-byte and scattered across several resolvers, and
identical query names are what let a resolver that already answered serve the
cached reply on a retry. Random-per-send pad would change the name every attempt
and defeat that cache. So **different cells get different lengths** (the spike
dissolves), while **the same cell is always byte-identical** (retransmit + scatter
stay cacheable). The pad rides inside the seal — the op plaintext is padded to the
deterministic length before sealing — and is harmless because every op is
fixed-length and self-delimiting (trailing pad is ignored; `DATA` uses its known
real-chunk length from the total). The one exception is `OP_FRAG`, whose chunk is
concatenated on reassembly, so fragment cells carry **no jitter** — they sit at
the budget floor, itself a real feed length. The remaining fingerprint is the
multi-cell *burst* of a send (§15).

**Cost.** Small `B` means more cells per send (≈1.6× at `B=10`, ≈3× at `B=6`);
the poll stays one cell. It is opt-in precisely because more queries is slower on
weak links.

---

## 9. Session handshake

A session is a fresh ephemeral x25519 key on the client side, ECDH'd against the
server's `ek`:

```
Ksession = HKDF( ECDH(eph_priv, ek_pub),
                 info = "thefeed-chat-session-v1" ‖ proto_ver ‖ query_key )
```

Mixing `query_key` into the HKDF info keeps the public passphrase a hard
requirement even though `ek` is published. Mixing `proto_ver` makes the
protocol version **tamper-evident** (see below).

The client streams a **handshake stream** across one or more handshake cells
(counter = chunk index), which the server reassembles:

```
stream = eph_pub(32) ‖ proto_ver(1) ‖ kind(1) ‖ sealed_bootstrap
sealed_bootstrap = SealChat(Ksession, setup_tag, BOOTSTRAP_COUNTER=0x400000, bootstrap_plaintext)
```

`eph_pub` and `proto_ver` are cleartext (the server must read `proto_ver`
*before* deriving `Ksession`, to pick the version's derivation); the bootstrap is
sealed.

**Version is bound to the key (downgrade resistance).** `proto_ver` is cleartext
but it is folded into `Ksession`. An on-path attacker who knows the *public*
passphrase can flip the byte, but then the server derives a different `Ksession`
than the client and the sealed bootstrap fails to open — the handshake aborts
(fail-closed). The attacker cannot force a silent downgrade to an
exploitable-but-supported version; at worst it is a denial of service, which any
on-path DNS attacker already has by dropping packets. The set of versions the
client is willing to choose comes from the **signed** `ChatInfo` (pinned key), so
the attacker cannot lie about which versions exist either. Knowing the public
passphrase is *not* the session secret — that needs `ek`'s private half (ECDH),
which the server alone holds — so a passphrase-only adversary cannot MITM.

Two kinds:

- **AUTH** (`kind=1`) — prove control of an already-registered account:
  ```
  bootstrap = addr(12) ‖ ts(4) ‖ proof(8)
  proof = HMAC( kss, "thefeed-chat-acct-proof-v1" ‖ eph_pub ‖ addr ‖ ts ‖ domain )[:8]
  kss   = HKDF( ECDH(enc_priv, ek_pub),
                info = "thefeed-chat-server-v1" ‖ client_enc_pub ‖ ek_pub )
  ```
  Only the account holder can compute `kss`, hence `proof`. The **domain** is
  bound into the proof, so a proof captured on one server cannot be replayed on
  another. `ts` is checked against server time within a skew window; a
  clock-skewed client is told the server's time and retries once.

- **REGISTER** (`kind=2`) — first contact: `bootstrap = RegisterEnvelope` (§6).
  The server verifies the signature, stores the record, and opens the session.

On success the server allocates a random 3-byte **session ref** (handshake bit
cleared), returns it sealed under `Ksession` (with the server's unix time and the
session TTL), and the client uses it as the `selector` for all subsequent
in-context cells.

`ensureSession` tries AUTH first; on `unknown_sender` it falls back to REGISTER.

### 9.1 `ek` rotation and session forward secrecy

`ek` is the single static key behind every `Ksession`, so a later compromise of
`ek_priv` plus recorded handshakes would let a passive observer recompute past
session keys and read the *session-layer* metadata (peer handles, op patterns,
sizes). Content is unaffected (separate `content_key`), but to bound that window
the server **rotates `ek` periodically** (default weekly):

- A new `ek` keypair is generated; the current key moves to a short-lived
  *previous* set and the new public key is re-published in the **signed**
  `ChatInfo`.
- New handshakes are tried against the current key, then any previous key still
  inside its grace window — so a client whose cached `ChatInfo` predates the
  rotation (clients refresh hourly) is never locked out.
- **Live sessions are unaffected:** `Ksession` is derived once at handshake and
  cached, never recomputed from `ek` per op.
- Past its grace the old private key is destroyed. An `ek_priv` captured *now*
  can no longer derive sessions from before the previous rotation — forward
  secrecy at the rotation granularity.

This does not give content forward secrecy (still §15-2); it caps the blast
radius of the `ek` SPOF for the session/control layer.

---

## 10. In-context operations

Op byte = `version<<4 | op` in the first sealed plaintext byte.

| Op | # | Plaintext fields | Purpose |
|----|---|------------------|---------|
| `INBOX_STATUS` | 1 | — | list waiting messages + quota |
| `INBOX_FETCH`  | 2 | `peer_handle(4) ‖ seq(4) ‖ block(1)` | fetch one envelope block |
| `ACK`          | 3 | `peer_handle(4) ‖ up_to_seq(3) ‖ receipt(6)` | confirm delivery, free inbox, sign ✓✓ (§12) |
| `KEY_FETCH`    | 4 | `addr(12)` | fetch a peer's registration record |
| `SEND_STATUS`  | 5 | `addr(12)` | read ✓/✓✓ counters + receipt for messages I sent |
| `SEND_START`   | 6 | `dst(12) ‖ total_len(2)` | begin an upload |
| `DATA`         | 7 | `idx(1) ‖ chunk(≤13)` | one body chunk |
| `FIN`          | 8 | `crc32(4)` | commit the upload |

Status codes (in the sealed response, first byte):

```
0  OK              1  unknown_recipient   2  inbox_full       3  pair_quota
4  rate_limited    5  unknown_sender      6  bad_version      7  busy
8  unknown_session 9  bad_auth           11  not_found       12  incomplete
13 replay          14 bad_request        15  disabled
```

**Responses** are sealed under `Ksession` with a response-side counter
(`req_counter | 0x800000`, kept disjoint from request nonces) and then wrapped by
the feed's normal passphrase response layer inside a TXT record. If the server no
longer knows the session (TTL expiry or reboot) it cannot produce a sealed reply,
so it returns a 1-byte **session-lost sentinel** (`0xE5`); the client
transparently re-handshakes and retries. `0xE5` is the one unauthenticated
message (the server can't seal it), so an on-path attacker can inject it to force
re-handshakes; the client therefore **rate-limits handshakes** (a minimum gap
between them), bounding the amplification a `0xE5` flood can cause while still
recovering promptly from a genuine reboot/expiry.

Counter discipline: in-context counters live below the reserved regions
(`0x400000` bootstrap, `0x800000` response bit). A client re-handshakes well
before exhausting the 22-bit space; the server rejects any in-context counter
≥ `0x400000`.

**No in-session anti-replay window — on purpose.** Cells are retransmitted
*verbatim* (same counter) on UDP loss, and the server processes each counter
**statelessly and idempotently** — that is what makes loss recovery work over
DNS. A reject-on-duplicate window would break legitimate retransmits, and buys
nothing: every op is idempotent, so replaying a captured cell merely re-runs the
same op to the same result. Keystream reuse is likewise a non-issue: a counter is
only ever reused for the *identical* plaintext (a retransmit), so no two distinct
plaintexts share a nonce.

---

## 11. Sending a message

### 11.1 Content encryption (end to end)

```
content_key = HKDF( ECDH(sender_enc_priv, recipient_enc_pub),
                    info = "thefeed-chat-content-v1" ‖ src ‖ dst ‖ seq )
```

The inner body is sealed with **AES-256-GCM under a fresh random 12-byte nonce
carried in the envelope**.

```
inner    = cflag(1) ‖ (deflate(text) | raw text)   ; cflag picks the smaller
envelope = ver(1)=1 ‖ seq(4) ‖ nonce(12) ‖ GCM_ciphertext ‖ srv_mac(8)
srv_mac  = HMAC( kss, "thefeed-chat-srvmac-v1" ‖ src ‖ dst ‖ seq ‖ SHA-256(nonce‖ct) )[:8]
```

- **Why a transmitted random nonce, not a fixed zero one.** `seq` is per-server,
  so the same `(src, dst, seq)` — hence the same `content_key` — recurs when you
  message the same contact on two servers (a normal use of the multi-server
  feature). A fixed nonce would then reuse the keystream across two different
  plaintexts (XOR of ciphertexts = XOR of plaintexts) and break GCM auth. A
  per-message random nonce makes each encryption unique *regardless* of `seq`
  reuse (the same principle behind Signal's per-message keys), so confidentiality
  never hinges on `seq` uniqueness. Cost: +12 bytes per message (≈ one extra
  fixed-size cell), and the wire stays uniform — every cell is still 25 bytes.
- The **sender address is not in the envelope.** The recipient derives
  `content_key` using the `src` from its inbox entry; a wrong `src` yields a wrong
  key and GCM fails. Misattribution is therefore impossible.
- `srv_mac` authenticates the envelope *to the server* (only the registered
  holder of `sender_enc_pub` can compute `kss`), binding the routing pair, the
  seq, and the exact nonce‖ciphertext. The server checks it at `FIN`, so a relay
  cannot flip the nonce undetected.

### 11.2 Chunked upload (selective-repeat)

The envelope is split into ≤13-byte chunks (one chunk min, even for empty):

```
SEND_START(dst, total_len)
   → server allocates a reassembler for this session; replies a bitmap of
     chunks it already has (empty on a fresh start).
DATA(idx, chunk)  ×N
   → server fills the reassembler; each reply carries the updated bitmap, so the
     client only resends missing chunks (DNS is lossy).
FIN(crc32)
   → server checks  len == total_len  &&  crc32(assembled) == crc32,
     parses the envelope, verifies srv_mac, then CommitMessage(src, dst, seq).
```

Integrity in depth: every cell is individually sealed (4-byte tag), the whole
body is CRC32-checked at FIN, and the envelope carries the 8-byte server MAC and
the GCM tag. The CRC is for accidental corruption; the MAC/GCM are the
cryptographic guarantees.

### 11.3 Sequence numbers and idempotency

Sequence is **per (pair, server)**, recoverable from the server (`SEND_STATUS`
returns the last accepted seq, so an amnesiac client resyncs). Commit is
idempotent by seq:

```
seq <  last_accepted  → replay        (rejected; client bumps to last_accepted+1)
seq == last_accepted  → OK            (idempotent re-commit; safe to retry)
seq == last_accepted+1 → accept
```

This makes the whole send retry-safe: a lost `FIN`-OK, a re-handshake mid-upload,
or a duplicate all converge to "delivered exactly once". On any transport error
the client reconciles via `SEND_STATUS` before re-sending.

---

## 12. Receiving and delivery state

```
INBOX_STATUS → quota ‖ [ (src(12), seq(4), len(2), blocks(1)) … ]
INBOX_FETCH(peer_handle, seq, block) → one block of that sender's envelope
ACK(peer_handle, up_to_seq) → frees src→me messages ≤ up_to_seq, bumps last_delivered
```

Both `INBOX_FETCH` and `ACK` carry a `peer_handle` — the first 4 bytes of the
sender's address. **It is required, not cosmetic:** `seq` is per-pair, so two
contacts can each have a pending message at the same `seq` (every new contact
starts at 1); without the handle the server would return the wrong sender's
envelope. The server resolves the handle to the full address within the caller's
own known pairs (a collision only ever names another of *your* contacts; on an
ambiguous handle the op returns `not_found` and the client retries).

**Delivery ticks** are per-pair-per-server counters the sender reads with
`SEND_STATUS`:

- `last_accepted` (✓) — the server stored the message. This is the server's word,
  but it only confirms an upload the sender already witnessed (the `FIN`-OK).
- `last_delivered` (✓✓) — the recipient fetched **and** ACKed it.

**✓✓ is end-to-end authenticated (the server cannot forge it).** A bare counter
would be the relay's unverifiable claim, so the ACK carries a 6-byte **receipt**:

```
receipt_key = HKDF( ECDH(my_enc_priv, peer_enc_pub), info = "thefeed-chat-receipt-v1" )
receipt     = HMAC( receipt_key, "thefeed-chat-receiptmac-v1" ‖ sender ‖ recipient ‖ up_to_seq )[:6]
```

`receipt_key` is the same pair ECDH the content uses, so only the two ends — never
the server — can compute it. The recipient signs its `up_to_seq` watermark; the
server stores `(up_to_seq, receipt)` and relays it in `SEND_STATUS`; the sender
recomputes the MAC and only shows ✓✓ if it verifies. `(sender, recipient)` are
bound in originator-first order (so a receipt can't be reflected onto the reverse
direction) and `up_to_seq` is bound (so an old receipt can't be replayed for a
higher seq). The server can still **withhold** a real receipt (it can drop any
data) — unavoidable for a relay — but it **cannot fabricate** a ✓✓. If the receipt
is absent or invalid the sender simply shows no ✓✓ (degrades to ✓), never a false
one. Cost: the 6-byte tag keeps both the ACK and the `SEND_STATUS` response inside
one cell.

Because seq is per-server, these counters (and receipts) are **per-server** too: a
message sent on server A keeps its ✓✓ even after the conversation switches to
server B.

**Receive-side replay.** The recipient keeps a per-(pair, server) high-water of
the seq it has acked, and drops any inbound message with `seq ≤` that watermark.
Acks are contiguous (a fetch gap withholds later seqs), so anything at/below the
mark was already delivered; this stops a malicious server re-serving an old,
authentic envelope to make it render twice — even after local history is cleared.

---

## 13. Server state and limits

Per account the server holds: the published registration record, a bounded
inbox of ciphertext envelopes, per-pair `(last_accepted, last_delivered)`, and
send-rate counters. Sessions live in RAM only.

Default limits (advertised in `ChatInfo`, operator-tunable):

| Limit | Default | Meaning |
|-------|---------|---------|
| chunk size | 13 | body bytes per DATA cell (advertised so clients size uploads) |
| max message bytes | 500 | per message plaintext |
| inbox cap | 50 | undelivered messages per account |
| per-pair cap | 10 | undelivered messages per (sender→me) pair |
| sends/hour | 30 | per account |
| session idle | 120 s | re-handshake after idle |
| session hard | 600 s | absolute session lifetime |
| message TTL | 72 h | undelivered messages expire |

**What the server learns:** routing pairs (who messages whom on this server),
message sizes and counts, timing, and the social graph of accounts registered on
it. **What it cannot learn:** message content. This metadata exposure is an
accepted cost (§15-5).

---

## 14. Cryptographic constructions (summary)

| Purpose | Construction | Key / info |
|---------|--------------|------------|
| Identity | ed25519 | HKDF(seed, "…identity-v1") |
| Enc key | x25519 | HKDF(seed, "…encryption-v1") |
| Address | SHA-256(id_pub)[:12] | — |
| Session key | HKDF(ECDH(eph, ek)) | "…session-v1" ‖ proto_ver ‖ query_key |
| Per-op seal | AES-256-CTR + HMAC-SHA256[:4] | enc/mac sub-keys of Ksession |
| Client↔server (kss) | HKDF(ECDH(enc, ek)) | "…server-v1" ‖ client_enc ‖ ek |
| Account proof | HMAC(kss)[:8] | "…acct-proof-v1" ‖ eph ‖ addr ‖ ts ‖ domain |
| E2E content | HKDF(ECDH(enc,enc)) | "…content-v1" ‖ src ‖ dst ‖ seq |
| Message body | AES-256-GCM, random 12-byte nonce (in envelope) | content_key |
| Server MAC | HMAC(kss)[:8] | src ‖ dst ‖ seq ‖ SHA-256(nonce‖ct) |
| Delivery receipt | HKDF(ECDH(enc,enc)) → HMAC[:6] | key "…receipt-v1"; MAC "…receiptmac-v1" ‖ sender ‖ recipient ‖ up_to_seq |
| Registration | ed25519 signature | "…register-v1" |

`ek` rotates periodically (default weekly); the rotated-out key authenticates
handshakes through a grace window, then its private half is destroyed (§9.1).

HKDF is HKDF-SHA256 throughout. All multi-byte integers are big-endian.

---

## 15. Design decisions & trade-offs

Deliberate choices and accepted trade-offs, recorded so the rationale isn't lost.
The protocol is pre-release, so incompatible improvements are still in scope
(§17) — but these are where we have landed, not open questions.

1. **4-byte per-op seal tag — accepted.** Forging a sealed cell costs ~2³²
   *online*, rate-limited DNS round-trips per `(selector, counter)`, against
   limits that are **per account** (the session cap and send-rate live on the
   account address, so the multi-domain spread does not multiply the budget). A
   forgery yields only E2E ciphertext; a forged *control* cell could corrupt
   delivery state, but the 2³² online cost bounds that. Cell space is tight (15
   plaintext bytes by default) and a longer tag costs payload, so 4 bytes stands.
2. **No content forward secrecy — by design.** `content_key` derives from
   long-term enc keys, so a stolen seed decrypts past messages still held on a
   server. This is the deliberate "lite messenger" trade-off: no per-message
   ratchet (X3DH / Double Ratchet) and the complexity it would add. The *session
   / metadata* layer does get forward secrecy from `ek` rotation (§9.1); message
   content does not.
3. **Authenticated delivery (✓✓).** ✓✓ was once a bare server-maintained counter
   a malicious relay could fabricate or suppress. It now carries an E2E pair-MAC
   **receipt** (§12): the server can no longer forge a ✓✓, only withhold one
   (which no relay can be stopped from doing). +6 bytes on the ACK / `SEND_STATUS`,
   still one cell.
4. **Random message nonce.** An earlier draft used a fixed zero GCM nonce, "safe
   iff `content_key` never repeats" — but `seq` is per-server, so the same
   `(src,dst,seq)` recurs across servers → keystream reuse. Fixed by carrying a
   random 12-byte nonce per message (§11.1). Residual: AES-GCM is not
   nonce-misuse-resistant, so a broken RNG would still be catastrophic; a
   nonce-misuse-resistant AEAD (AES-GCM-SIV) would harden this at zero wire cost
   and is a candidate for a later version.
5. **Server metadata exposure — accepted cost.** The relay sees the routing graph
   (who messages whom on it), message sizes, and timing — never content. This is
   the unavoidable cost of store-and-forward through a third party; we document it
   rather than mitigate it. (Sealed-sender, cover traffic, or pseudonymous
   mailboxes stay theoretically possible but out of scope.)
6. **`peer_handle` = 4 bytes.** `INBOX_FETCH` / `ACK` / `SEND_STATUS` name the
   peer by the first 4 bytes of its address (the full 12 would not fit a cell
   alongside seq+block). A prefix collision can only ever match another of *your
   own* contacts; the server returns `not_found` and the client retries. Accepted.
7. **Address = 96 bits (12 bytes).** The largest that fits the single-cell control
   ops (`SEND_START` = op+addr+len = exactly 15 plaintext bytes at the default
   budget). Hijacking a specific address is a second-preimage = 2⁹⁶ *with an
   elliptic-curve keygen per attempt* — infeasible for any attacker; the only
   erosion is multi-target, which a niche DNS messenger never reaches. 96 bits
   chosen deliberately.
8. **Account-proof freshness.** The AUTH proof binds the chat domain and a
   timestamp checked against server time within a skew window; a clock-skewed
   client is told the server's time and retries once. Domain binding blocks
   cross-server replay; the skew window bounds any within-server replay to its
   width.
9. **Open registration + per-account limits.** Registration is self-signed and
   free; abuse is bounded by per-account rate limits (sends/hour, inbox/pair caps,
   concurrent-session cap). No proof-of-work or invite gate — kept simple; the
   limits can tighten if abuse appears.
10. **Versioning scaffolding.** Three hooks exist: the signed `ChatInfo` min/max
   version range, the op-byte version nibble, and the handshake `proto_ver` byte
   bound into `Ksession` (downgrade-resistant). A real v1→v2 migration would add
   (a) the server running two derivations at once and (b) envelope/register
   parsers dispatching by version instead of single-accept. The scaffolding is in
   place; the dispatch is built when a v2 actually exists.
11. **Traffic shape vs. the feed.** §8.2's configurable cell size plus
   **deterministic per-cell jitter** put chat *inside* the feed's length spread
   (deterministic so retransmits/scatter stay cache-stable — random-per-send pad
   would break that). The one remaining tell is the **burst** of a multi-cell send
   (N queries, a different shape than a feed read); pacing it or riding the feed's
   cover traffic is a planned later layer.

---

## 16. Security considerations (collected)

- **Trust anchoring:** all server-provided material (`ek`, limits, domains) is
  authenticated under the pinned feed-signing key. Pin distribution (the
  `thefeed://…?sk=` URI) is out of band and the weakest link — a bad pin is a bad
  everything.
- **Key substitution:** defeated by `address = hash(identity_pub)` plus the
  recipient re-checking it on every key fetch.
- **Cross-server replay:** the account proof binds the chat domain.
- **Authenticated delivery:** ✓✓ carries an E2E pair-MAC receipt — the relay can
  withhold it but cannot fabricate one (§12).
- **Receive-side replay:** a per-pair acked high-water drops an old envelope a
  malicious relay re-serves, even after local history is cleared (§12).
- **Session forward secrecy:** `ek` rotates; the old private half is destroyed
  after a grace window, bounding an `ek` compromise to one rotation (§9.1).
- **Handshake-flood bound:** the injectable `0xE5` sentinel can force a
  re-handshake, but the client rate-limits handshakes to cap the amplification.
- **Fail-closed everywhere:** unverifiable `ChatInfo`, an unknown session, or a
  bad seal all stop the operation rather than degrading.
- **Compression:** inbound bodies are size-capped before inflation to bound a
  decompression-bomb to memory.

---

## 17. How to comment

Found a **security issue**, or have a **concrete** wire-level improvement? Open an
issue or PR against this file, referencing the section and giving the byte layout.
Because the protocol is pre-release, **incompatible** improvements are in scope.
