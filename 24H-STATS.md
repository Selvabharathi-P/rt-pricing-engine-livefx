# How the 24h High, Low and % Change Are Computed

The engine computes these three numbers itself, from the ticks it has
received. The upstream feed's own 24h fields (`high`, `low`, `percentage`,
or `low_24h` / `high_24h` / `change_24h` in the brief) are **never used**.

## How it works

### 1. Mid price

Every tick has a buy (ask) and a sell (bid). The 24h statistics use the
midpoint:

```
mid = (buy + sell) / 2        rounded to 8 decimals
```

Buy and sell themselves are passed through to clients unchanged.

### 2. One-minute buckets

Each symbol keeps a ring of **1,440 buckets**, one per minute for 24 hours.
Each bucket records that minute's mid prices:

| Field | Meaning |
|---|---|
| open | mid of the earliest tick in the minute |
| high / low | highest and lowest mid in the minute |
| close | mid of the latest tick in the minute |
| count | number of ticks |

### 3. The window

"Now" is the minute of the newest tick received. This is event time, not the
server's clock. The window is the **last 1,440 minutes, including the
current one**. Anything older drops out.

### 4. The three numbers

```
24h high   = highest bucket high in the window
24h low    = lowest bucket low in the window
24h change = (current mid − open of the oldest bucket in the window) / that open × 100
             rounded to 4 decimals
```

### 5. Messy data

- **Duplicates** (same timestamp, buy and sell) are dropped.
- **Late ticks** go into the minute they belong to, so they still count for
  high and low. They never overwrite the current buy and sell.
- **Ticks older than 24h** are rejected.

### Cost

Each tick updates one bucket, which is constant work. Once a minute the
engine rescans the 1,440 buckets to drop the expired minute. Memory is fixed
at about 73 KB per symbol, whatever the tick rate.

---

## Example: EURUSD

Ticks received:

| Time | Buy | Sell | Mid | Note |
|---|---|---|---|---|
| Day 1, 10:00:30 | 1.1004 | 1.1000 | 1.1002 | exactly 24h old at "now", so **outside** the window |
| Day 1, 10:01:15 | 1.1012 | 1.1008 | **1.1010** | oldest minute in the window: the **reference** |
| Day 1, 15:30:00 | 1.1052 | 1.1048 | **1.1050** | highest |
| Day 2, 03:12:40 | 1.0962 | 1.0958 | **1.0960** | lowest |
| Day 2, 10:00:05 | 1.1032 | 1.1028 | **1.1030** | latest tick, so "now" is Day 2 10:00 |

The window covers **Day 1 10:01 to Day 2 10:00** (1,440 minutes). The Day 1
10:00 bucket falls outside it.

| Output | Value | How |
|---|---|---|
| buy / sell | 1.1032 / 1.1028 | latest tick, unchanged |
| 24h high | 1.1050 | Day 1 15:30 |
| 24h low | 1.0960 | Day 2 03:12 |
| 24h change | **+0.1817 %** | (1.1030 − 1.1010) / 1.1010 × 100 = 0.18165… → 0.1817 |

The client receives this row, with fields `[symbol, buy, sell, high24h, low24h, change24hPct, ts]`:

```json
["EURUSD", 1.1032, 1.1028, 1.105, 1.096, 0.1817, 1792...]
```

### One minute later: the window rolls

A tick arrives at **Day 2 10:01:20** with mid **1.1034**:

- The window now starts at Day 1 10:02, so the 10:01 bucket (1.1010) drops
  out.
- The new reference is the next-oldest bucket: Day 1 15:30, open 1.1050.
- Change = (1.1034 − 1.1050) / 1.1050 × 100 = **−0.1448 %**.
- High and low stay at 1.1050 and 1.0960, until those minutes themselves pass
  the 24h mark.

### A late tick arrives

A tick stamped **Day 2 09:58:00** with mid **1.0950** arrives *after* the
10:00 tick:

- It goes into the 09:58 bucket, so the **24h low becomes 1.0950**.
- Buy and sell stay at the 10:00 values, because that tick is newer.

### A duplicate arrives

The Day 2 10:00:05 tick is received again after a reconnect. It has the same
timestamp, buy and sell, so it is **dropped**. Nothing changes and nothing is
sent to clients.

---

## In the code

| Step | Where |
|---|---|
| Mid price, % change, rounding | `internal/engine/engine.go`: `midPrice`, `refresh`, `round4` |
| Buckets, window, high/low/reference | `internal/engine/window.go`: `add`, `recompute`, `stats` |
| Duplicates, late and stale ticks | `internal/engine/engine.go`: `Apply` |
| Proof against a brute-force calculation | `TestAgainstBruteForce` in `internal/engine/engine_test.go` (20,000 random ticks, 5% late, over 2.3 days) |

## Trade-offs

- **Minute granularity.** The window edge is accurate to the minute: a price
  leaves the stats once its whole minute is more than 24h old. A tick-exact
  window would have to keep every tick, which uses much more memory and is
  costly to persist.
- **Mid, not bid or ask.** Highs and lows are on the mid price, so a buy
  (ask) can sit slightly above the 24h high. If bid-based or ask-based stats
  are required, it's a one-line change in `midPrice`.
- **Cold start.** With no saved history, "24h" change covers only the time
  since the engine started, until a full day has been collected. Saved
  history (Redis) carries the window across restarts.
