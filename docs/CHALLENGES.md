# Challenges & Solutions

These are the real problems I ran into while building the pricing engine:
what happened, how I noticed, what I did, and what I took away from it.
The technical summary is in the [README](../README.md#challenges--solutions).

---

## 1. The live feed didn't look like the brief

The brief said each price message has `buy`, `sell` and `timestamp`. I built
the parser to that, tested it against my simulator, and everything passed.
On the first live run the engine received **17,000 messages and produced
zero prices**, and no errors either. The messages were valid JSON; they just
had different field names (`bid`/`ask`, and no timestamp).

**What I did:** I added a counter and log samples for "messages that parsed
but contained no prices". Within seconds that showed the real format, and I
mapped `ask` → buy and `bid` → sell.

**Lesson:** a spec isn't the system. Make silent failures loud before you
need them.

## 2. Locked out by my own connection

LiveFXHub allows **one session per API key**. While debugging, the server
kept answering *"already have an active session"*, and there was no logout I
could call without logging in first. I couldn't even open a second
connection to inspect messages, so I had to debug through the running
engine's own logs.

**What I did:** I treated "session busy" as its own case and retried slowly
(every 30–40 s) instead of hammering. I also made every shutdown send a
proper close message, so the server releases the session immediately.

**Lesson:** when you can't reproduce on demand, observability inside the
running process is all you have.

## 3. Test data leaked into real numbers

When I switched from the simulator to the live feed, the engine "helpfully"
restored the simulator's saved price history. The live 24-hour high and low
would have been built on fake prices, and nothing would have flagged it.

**What I did:** I separated the saved data per price source, so live and
simulator data can never mix.

**Lesson:** anything that remembers state across restarts also remembers
your mistakes.

## 4. Exact 24-hour window versus a fast one

The obvious design keeps every tick for 24 hours. That's heavy on memory,
and costly to save after every change.

**What I did:** I keep 1,440 one-minute buckets per symbol:
- every tick is constant work;
- memory is fixed;
- the buckets double as candles.

The cost is that the window's edge is accurate to a minute, not a
millisecond. I wrote a test that compares it against a slow brute-force
calculation over 20,000 random ticks to prove the numbers match.

**Lesson:** choose the trade-off on purpose, then prove it with a test.

## 5. One slow client could hurt everyone

Pushing every update to every client means a slow client either builds a
huge backlog or blocks everyone else.

**What I did:** each client holds at most the latest price per symbol. If it
falls behind, it simply gets the newest price, which is what a price screen
needs anyway.

**Lesson:** for live prices, "latest" beats "complete".

## 6. "The chart doesn't open when I click"

My automated test clicked rows and the chart opened, so I reported it fixed.
The user still couldn't open it. The table was rebuilding itself about 25
times a second. A real click is a press plus a release; if the row moves in
between, the browser cancels the click. My test used an instant synthetic
click, so it never saw the problem.

**What I did:** rows now update in place and only move when the order truly
changes. I retested with real mouse press and release events: 8 of 8 clicks
worked, where 0 of 8 had before.

**Lesson:** test the way users act, not the way that's convenient. And when a
user says it's still broken, believe them over the test.

## 7. The chart broke after 15 minutes

The live table kept streaming, but the chart went blank. The login token
expires after 15 minutes. The WebSocket survived, but the chart's data
requests started failing quietly.

**What I did:** I routed those requests through the same helper that
silently refreshes the session.

**Lesson:** test what happens after time passes, not just the first five
minutes.

## 8. Security that looks right but isn't

It's tempting to trust a "role" sent by the browser, or to keep the
encryption keys next to the data they protect.

**What I did:**
- the role is derived on the server from KYC status on every request;
- access is re-checked while sockets are open, so a rejected merchant is
  cut off immediately;
- API secrets are stored only as hashes;
- the keys live in a separate storage volume from the data.

My test reads the raw database to prove no personal data is stored in plain
text. It once passed falsely: the tool I used to read the database cut long
values short. I changed it to read every value in full.

**Lesson:** a security test that can't fail is worse than none.

## 9. "Works on my machine" with Windows

- A WebSocket close produced a connection reset on Windows that Linux never
  showed.
- The backup restore script worked when I tested it from one shell, then
  failed on another machine in Windows PowerShell, which turned a harmless
  "volume not found" message into a fatal error.

**What I did:** I fixed both, and retested the script in the exact shell the
user ran.

**Lesson:** run the final check in the environment the person will actually
use.

---

## The common thread

Most of the hard problems weren't algorithms. They were gaps between what I
assumed and what actually happened:

- the spec versus the real feed;
- synthetic clicks versus real ones;
- my shell versus the user's.

What helped every time was making failures visible, then testing against
reality.
