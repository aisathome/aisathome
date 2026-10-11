---
title: Credit
description: How AIs@home counts credit for the GPU time a node gives, in BOINC's unit, and what credit is not.
---

Your node earns **credit** for the answers it computes for other people. Credit is
recognition: a number on the [network page](/network/) and in statistics files in the
format BOINC projects use. **It is not money**, there is no token, and it cannot be
exchanged for anything. AIs@home is not affiliated with or endorsed by BOINC or any BOINC
project; we only use the same unit and file format.

## How much

Credit follows the **GPU time** a node gives. It is anchored to BOINC's unit, the
cobblestone (1/200 of a day on a computer doing one billion operations a second): generating
one token of the 27-billion-parameter model takes about 54 billion operations, which is
0.125 credit.

| Per token | Credit | Why |
|---|---:|---|
| Output (generated) | 0.125 | Generating is what keeps a GPU busy: one token at a time |
| New prompt | 0.0125 | Reading a prompt goes about ten times as fast per token or more |
| Cached prompt | 0.00125 | Your node just read it for the same person and keeps it in memory |

A node that generates one answer at a time, all day without a pause (about 46 tokens a
second on an RTX 3090), would earn roughly 500,000 credit a day. Real nodes earn far less:
they wait for requests most of the time.

Credit is not comparable between projects; BOINC says the same about its own projects.

## Who counts it

The **gateway** counts every token itself, with the model's own tokenizer: the prompt it sent
your node and the answer it passed on. What a node reports about itself is not used. A
prompt counts as cached up to the part your node served the same person within the last ten
minutes; the node's own claim about its cache is ignored.

No credit for:

- answers to your own account (your own chats and API keys on your own node);
- answers that failed, or that a safety check stopped.

## Today, total and recent average

Your [account page](/account) shows credit for today (UTC), in total, and the **recent
average**: BOINC's formula, credit per day with a half-life of one week. It follows the
rate your node earns at, and fades when the node is off.

## Public or anonymous

You are **anonymous by default**: your credit counts in the totals only, and nobody sees
your name or an id. To appear on the leaderboard and in the statistics files, set a
**username** on your account page. Setting it is your consent to show it publicly with your
credit; clear it to disappear again from the next update. Usernames are unique, 3 to 32
characters, and cannot be web addresses.

## Statistics files

The gateway publishes credit in BOINC's statistics format (the files BOINC's `db_dump`
writes) at [`/stats/`](/stats/): `tables.xml`, `user.gz` (volunteers with a username),
`host.gz` (their nodes, by number) and `team.gz` (no teams yet). They are rebuilt at most once
an hour. BOINC statistics sites could read them; none does yet.

## Not yet

- **No cross-project ids (CPIDs)** and so **no Gridcoin**. Linking a BOINC identity needs a
  proof that it is yours; a typed-in CPID proves nothing. This comes later, if at all.
- **No checks of answers yet.** Credit is for work the gateway saw pass through, not proof
  that an answer was right. Like any leaderboard, it can be gamed, for example with a second
  account; that is one reason it stays recognition only.
