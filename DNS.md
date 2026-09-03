# DNS guide for Carteiro

Everything you need to configure DNS so the emails Carteiro sends land in the
inbox (not spam), explained step by step. The [README](README.md) has a
short checklist; this file goes deeper into each record, why it exists, and
how to debug when something is wrong.

Carteiro **only sends** — it is a submission relay, not a receiving server.
Everything below is about making the *outbound* side of your domain trusted.

**Conventions used here:** `smtp.example.com` = Carteiro's public hostname
(the `hostname:` config value, shown in EHLO/banner), `203.0.113.10` = its
fixed public IP, and `example.com` = the sending domain (the part after `@`
in the accounts). Replace them with yours.

---

## Table of contents

1. [How receiving servers decide: SPF + DKIM + DMARC + PTR](#1-how-receiving-servers-decide)
2. [SPF — which IPs may send for the domain](#2-spf)
3. [DKIM — signing the messages](#3-dkim)
4. [DMARC — the policy that ties it together](#4-dmarc)
5. [PTR / reverse DNS — who owns the IP](#5-ptr--reverse-dns)
6. [MX — only if you want bounces back](#6-mx)
7. [Cloudflare-specific notes](#7-cloudflare-specific-notes)
8. [Verification commands](#8-verification-commands)
9. [Troubleshooting table](#9-troubleshooting-table)
10. [Final checklist](#10-final-checklist)

---

## 1. How receiving servers decide

When your Carteiro server delivers a message, the receiving side (Gmail,
Outlook, etc.) runs up to four checks. A message that fails the important
ones is spam-foldered or rejected:

| Check | What it verifies | Record needed |
|---|---|---|
| **SPF** | The connecting IP is authorized to send **for the envelope sender domain** | TXT on the sender domain (e.g. `example.com`) |
| **DKIM** | The message carries a valid **signature** by a key published for the sender domain | TXT at `<selector>._domainkey.<sender-domain>` |
| **DMARC** | SPF and/or DKIM **align** with the visible `From:` header domain, and a policy says what to do when they fail | TXT at `_dmarc.<sender-domain>` |
| **PTR/rDNS** | The **outbound IP** resolves back to a name (ideally the one the server announces in EHLO) | Set at the **IP owner** (hosting provider), not in your DNS zone |

Two subtle but crucial points:

- **SPF/DKIM/DMARC live on the *sender* domain, not on the server hostname.**
  If the account is `no-reply@example.com` but Carteiro runs on
  `smtp.anotherdomain.com`, all three records are published **on
  `example.com`**. The `smtp.anotherdomain.com` name is only used for EHLO
  and the PTR.
- **Alignment matters.** DMARC compares the domain of the *visible From:*
  header with the domain that passed SPF (the envelope sender) or signed the
  DKIM. Carteiro forces `MAIL FROM` to be an account of the configured
  domain, so if you send `From: no-reply@example.com` and the account is
  `no-reply@example.com`, alignment is guaranteed by construction.

---

## 2. SPF

### 2.1 What it is

A TXT record on the sender domain listing the IPs allowed to send for it.
When Carteiro connects to the recipient's MX, SPF compares the **source IP
of that TCP connection** against the list.

### 2.2 The classic record

```dns
example.com.  TXT  "v=spf1 ip4:203.0.113.10 -all"
```

| Part | Meaning |
|---|---|
| `v=spf1` | version marker, always first |
| `ip4:203.0.113.10` | allow this IP to send (0 DNS lookups) |
| `-all` | reject everything else (hard fail). `~all` = soft fail (spam folder). **Never** `+all` |

### 2.3 Mechanisms (it does not have to be an IP)

| Mechanism | Example | Cost in lookups | When to use |
|---|---|---|---|
| `ip4:` / `ip6:` | `ip4:203.0.113.10` | 0 | fixed IP — the recommended default |
| `a:` | `a:smtp.example.com` | 1 | the A record of that name resolves to the IP; convenient when the IP can change |
| `include:` | `include:spf.provider.com` | 1+ | delegate part of the policy to a provider you already use |
| `mx:` | `mx` | 1 | the domain's MX IPs may also send (rarely what you want for a relay) |

### 2.4 Rules and pitfalls

- A bare domain is **not a valid mechanism**: `v=spf1 example.com -all`
  makes the whole record invalid. Always use the keyword: `a:example.com`.
- SPF allows at most **10 DNS lookups** per evaluation. `ip4:` costs 0;
  every `a:`/`include:`/`mx:` costs 1. Count them when you add providers.
- **Editing a domain that already has SPF**: do not create a second SPF
  record — **merge into the existing one**. Two `v=spf1` records on the same
  name is an error (receivers typically treat it as a permanent error, i.e.
  SPF fail). Keep the existing `include:` terms and add yours.
- A domain may have several TXT records (e.g. Google site verification) —
  only **one** may start with `v=spf1`.
- If you also send over IPv6 add it: `v=spf1 ip4:203.0.113.10 ip6:2001:db8::1 -all`.

### 2.5 Example: adding Carteiro to a domain with existing providers

Existing: `"v=spf1 include:spf.hostmar.com include:mailgun.org -all"`
(someone already sends through two providers).

To add Carteiro's server:

```dns
example.com.  TXT  "v=spf1 include:spf.hostmar.com include:mailgun.org a:smtp.example.com -all"
```

or, with an explicit IP:

```dns
example.com.  TXT  "v=spf1 include:spf.hostmar.com include:mailgun.org ip4:203.0.113.10 -all"
```

Both are equivalent when `smtp.example.com` resolves to `203.0.113.10`.
`a:` is friendlier to read; `ip4:` is explicit and costs zero lookups.
Change only the **value** of the existing SPF record — keep the other TXT
records untouched.

### 2.6 Verify

```bash
dig +short TXT example.com
```

---

## 3. DKIM

### 3.1 What it is

A cryptographic **signature** added to each message by Carteiro, using the
private key stored in its database. Receivers fetch the matching **public**
key from DNS and verify the signature. A valid signature proves the message
was not altered and came from someone holding the private key.

The signature is stored in a `DKIM-Signature:` header that says which
selector and domain were used, e.g. `s=mail; d=example.com`. Receivers then
look up `mail._domainkey.example.com`.

### 3.2 Generate the key (RSA-2048) and publish the public part

```bash
openssl genrsa -out dkim-example.key 2048
chmod 600 dkim-example.key

# public half, base64 one-liner (this is the p= value for DNS)
openssl rsa -in dkim-example.key -pubout -outform DER | openssl base64 -A
```

Publish at the sending domain:

```dns
mail._domainkey.example.com.  TXT  "v=DKIM1; k=rsa; p=<the base64 above>"
```

Give the **private key** to Carteiro as a seed (YAML `dkim:` list or
`CARTEIRO_DKIM_KEYS=example.com:mail:<base64-of-whole-PEM>`), always as the
base64 of the whole PEM file so it fits one line. See the main
[README](README.md#keys-and-certificates-as-base64).

### 3.3 Choose a selector

- The selector is just a name you pick for the DNS lookup. `mail` is the
  convention. Anything works (`dkim2026`, `s1`, ...).
- Use **different selectors for different systems** on the same domain so
  they do not overwrite each other, and so you can rotate a key without
  breaking the old one while DNS propagates.
- If you send from the same domain through a newsletter provider, keep their
  selector and yours side by side — they are separate records.

### 3.4 Long TXT records and copy-paste corruption (very common)

A DKIM TXT value is ~450 characters, but a single DNS TXT *string* is
limited to 255 characters. Two consequences:

1. **Long values are normal.** When you `dig`, you will see the record split
   into two quoted chunks. That is expected — receivers concatenate the
   chunks. Do not "fix" it.
2. **Copy-paste can silently corrupt the value.** Line wrapping or invisible
   characters (e.g. backspace `0x08`) can end up *inside* the record. The
   record still looks fine in `dig` but verification fails. If DKIM does not
   validate after publishing:
   - delete the record and re-create it from a plain text editor
     (no smart quotes, no auto-wrapping);
   - confirm the value is a **single logical line** when you paste it;
   - after saving, compare what DNS returns with what you intended (see
     [Verification](#8-verification-commands)).

### 3.5 Verify

```bash
dig +short TXT mail._domainkey.example.com
```

Better: send a real message and test the signature end to end with
[dkimvalidator.com](https://dkimvalidator.com) or
[mail-tester.com](https://www.mail-tester.com).

---

## 4. DMARC

### 4.1 What it is

A TXT record at `_dmarc.<domain>` that tells receivers: *"when SPF or DKIM
fail and/or do not align with the From: domain, do this."* Without DMARC,
receivers each apply their own private rules (usually = spam folder).

### 4.2 Start safe, then harden

```dns
_dmarc.example.com.  TXT  "v=DMARC1; p=none; rua=mailto:postmaster@example.com; pct=100"
```

| Tag | Meaning |
|---|---|
| `p=none` | no enforcement yet — only send reports (start here) |
| `p=quarantine` | failed mail goes to spam |
| `p=reject` | failed mail is rejected outright (only after weeks of clean reports) |
| `sp=` | policy for subdomains (e.g. `sp=reject`) |
| `rua=` | where aggregate reports go |
| `pct=100` | percentage of messages the policy applies to |

After a week or two of `p=none` reports showing SPF/DKIM aligned, move to
`p=quarantine`, then eventually `p=reject`. If your domain already has a
DMARC record, **leave it** unless you know what you are doing.

### 4.3 Alignment (why Carteiro makes this easy)

DMARC passes when **SPF or DKIM align** with the visible `From:` domain:

- SPF aligns if the envelope sender domain equals (or is a subdomain of)
  `From:` — relaxed mode. Carteiro sets envelope sender = the account, so
  `From: no-reply@example.com` sent through account
  `no-reply@example.com` aligns.
- DKIM aligns if `d=` in the signature equals (or is a subdomain of)
  `From:`. Carteiro signs with the account's domain, so this aligns too.

One of the two passing+aligned is enough for DMARC to pass.

### 4.4 Verify

```bash
dig +short TXT _dmarc.example.com
```

---

## 5. PTR / reverse DNS

### 5.1 What it is — and who creates it

Forward DNS maps *name → IP* (your `A` record). **Reverse DNS maps IP →
name**. It is *not* a record you add to your DNS zone: only the owner of the
IP address block can create it. For a VPS that is your hosting provider's
panel or API (e.g. Contabo, Hetzner, DigitalOcean), never Cloudflare.

### 5.2 Why it matters for email

When your server connects to a recipient MX, it announces itself with EHLO
using Carteiro's `hostname:` value. The recipient then asks *"who is
203.0.113.10?"* and compares the answer with the announced name and the
`A` record:

```
PTR:  203.0.113.10  ->  smtp.example.com      (set at the IP owner)
A:    smtp.example.com -> 203.0.113.10        (your DNS zone)
EHLO: smtp.example.com                        (Carteiro hostname config)
```

When all three match it is called **FCrDNS** (forward-confirmed reverse
DNS). Receivers treat a missing or mismatched PTR as a strong spam signal;
some (Outlook/Exchange Online is the strictest) reject outright with
`550 5.7.1 ... no reverse DNS`. Google is more tolerant when SPF+DKIM pass,
which is why a test to Gmail can succeed while the same setup fails
elsewhere — fix the PTR anyway.

### 5.3 How to configure

In the VPS panel, look for **"Reverse DNS" / "rDNS" / "PTR"** on the IP and
set it to Carteiro's public hostname (`smtp.example.com`). Propagation can
take from minutes to a few hours.

### 5.4 Verify

```bash
dig -x 203.0.113.10 +short    # must print smtp.example.com.
```

---

## 6. MX

Carteiro does not receive mail, so **no MX is required to send**. Add one
only if you want bounces and DMARC reports delivered somewhere:

```dns
example.com.  MX  10 mail.example.com.
```

If the domain already has MX records (existing mail service), leave them —
they do not conflict with sending through Carteiro.

---

## 7. Cloudflare-specific notes

Many Carteiro users run DNS on Cloudflare. Three things to know:

1. **Mail records must be "DNS only" (grey cloud), never proxied (orange
   cloud).** Cloudflare's proxy only speaks HTTP/HTTPS on ports 80/443.
   SMTP runs on ports 25/465/587 — the proxy cannot forward it, and an
   orange-clouded `A` record will make SMTP connections fail or behave
   erratically. Always create the `A` record for the SMTP hostname with the
   proxy **off**.
2. **Cloudflare's TLS certificates are for HTTP.** They do not cover the
   SMTP listener on your server. The STARTTLS certificate lives inside
   Carteiro (`tls.cert_data`/`key_data` or `CARTEIRO_TLS_CERT`/`KEY`), not
   at the Cloudflare edge.
3. **You cannot set a PTR in Cloudflare.** Reverse DNS is created by the IP
   owner (your VPS provider), as explained in [section 5](#5-ptr--reverse-dns).

---

## 8. Verification commands

```bash
# SPF: one v=spf1 record on the sender domain
dig +short TXT example.com

# DKIM: public key for selector "mail"
dig +short TXT mail._domainkey.example.com

# DMARC policy
dig +short TXT _dmarc.example.com

# A record of the SMTP hostname
dig +short A smtp.example.com

# PTR (reverse) — from any machine
dig -x 203.0.113.10 +short

# count SPF lookups quickly (rough check of your record)
dig +short TXT example.com | grep -oE '(a:|include:|mx)' | wc -l
```

End-to-end tests (recommended before real volume):

- [mail-tester.com](https://www.mail-tester.com) — sends you a score with
  every check explained.
- [dkimvalidator.com](https://dkimvalidator.com) — tests the DKIM signature
  specifically.
- [MXToolbox SPF lookup](https://mxtoolbox.com/spf.aspx) — parses and
  validates your SPF syntax/lookups.

---

## 9. Troubleshooting table

| Symptom | Most likely cause | Fix |
|---|---|---|
| `550 ... no reverse DNS` | PTR missing/mismatched | Set PTR at the IP owner; match EHLO hostname and `A` record |
| Landed in Gmail spam | PTR wrong **or** missing DMARC | Fix PTR, then align SPF/DKIM and publish DMARC |
| `SPF fail (permerror)` | two `v=spf1` records on the same name | merge into one record |
| SPF fails but record looks right | source IP not listed, or record >10 lookups | add the relay IP (`ip4:` or `a:`); trim `include:`s |
| DKIM signature invalid | public key in DNS ≠ private key in Carteiro | re-check `p=`; regenerate and re-publish |
| DKIM invalid but key matches | TXT record corrupted by paste (invisible chars) | delete and re-create the TXT cleanly (see 3.4) |
| DKIM record "too long" at the registrar | value >255 chars in one string | registrar handles long TXT automatically; if not, split into quoted chunks of ≤255 |
| Message delivered but "signed by" absent | envelope-from domain has no DKIM seed | add the DKIM seed for the account's domain (seed or `POST /dkim`) |
| Timeout connecting to recipient | outbound port 25 blocked by provider | enable outbound 25 (AWS/GCP/etc. block it by default) |

---

## 10. Final checklist

Before sending real volume:

- [ ] Outbound port 25 allowed by the provider
- [ ] `A` record for the SMTP hostname → server IP (DNS only / grey cloud)
- [ ] PTR at the IP owner: IP → SMTP hostname (matches EHLO)
- [ ] SPF published on the **sender** domain, merged into the existing one
- [ ] DKIM key seeded in Carteiro and public `p=` published at
      `<selector>._domainkey.<sender-domain>`
- [ ] DMARC published on the sender domain (`p=none` first)
- [ ] Test with mail-tester.com and a real Gmail/Outlook address
- [ ] **Warm up** a new IP: a handful of messages per day at first, growing
      slowly

Remember the mental model: **SPF and DKIM live on the domain you send from;
PTR lives on the IP you send from; DMARC is the referee that checks both
against the `From:` you show.**
