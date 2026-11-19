---
title: Privacy
description: Who handles your data on AIs@home and its BOINC project, and what a volunteer's GPU can see.
---

*Last changed: 2026-11-19.*

AIs@home is an experiment: open AI models running on volunteers' GPUs instead of a big company's data centre. This page
covers the chat (chat.aisathome.org), the API, the website, running a node, and our BOINC project
(boinc.aisathome.org). Read it before you type anything you care about.

## Who is responsible

JFR Holding AS (org. no. 937 566 042), contact@aisathome.org.

JFR Holding AS is the controller under the GDPR for everything on this page. Write to the address above about
anything here, including the requests under "Your rights".

## The short version

- **Hardened, not confidential.** Our software doesn't show or save your conversation on the volunteer's machine, but
  the machine is theirs: someone who sets out to can read what their GPU is answering. We make keeping it harder, less
  valuable, traceable and punishable.
- Nodes never get your name, email address, account or IP address. What you write in the conversation does reach them.
- Your chats can be answered on a volunteer's machine anywhere in the world.
- We don't sell data, show ads, train models on your chats, or run tracking.
- **Don't put personal data (yours or other people's) into chats: answers are computed on volunteers' machines.** That
  is a rule of our [terms](/about/terms/). Don't paste passwords or secrets either.

## Where your message goes

Your message travels encrypted to our gateway, a server in Stockholm (Amazon Web Services, region eu-north-1). The
gateway passes it to a volunteer's computer (a "node"), which runs the model and sends the answer back the same way.
The gateway stores no conversation text apart from the evidence of a safety check (below); only chats you choose to
save are kept (below).

## What a node sees

**Don't put personal data (yours or other people's) into chats: answers are computed on volunteers' machines.** Our
terms forbid it. We know people will sometimes do it anyway, so the rest of this section says what happens then.

- A node gets the text of the conversation and the model settings, nothing else: not your account, name, email address
  or IP address. What you write in the conversation itself does reach it.
- Every turn carries the whole conversation so far, so the node sees all of it, not only your last message.
- We replace well-known secret formats (API keys and tokens of common services, private keys, webhook addresses) before
  a node sees them. Passwords and unusual formats are not caught: don't paste them.
- Messages that look like personal data (an email address, a payment card number, an IBAN or a Norwegian national
  identity number) go only to our own nodes ("trusted nodes"). Trusted means run by us, not secure: some are rented
  machines, whose host could in principle see what passes through. Detection is incomplete: names, addresses and health
  details are not detected and can reach volunteers' nodes.
- We try to give no single volunteer more than a third of the conversations, but that is not a guarantee: when no
  other suitable node is free, the request goes to whichever suitable node is, so while few nodes are online one
  volunteer can get every conversation.
- If a node fails before it answers, the gateway sends the conversation to another node, which can be another
  volunteer's. A conversation can therefore reach more than one node.
- Agents (requests with tools, for example opencode) go to volunteers' nodes only if your API key allows it.
- The node software keeps recent conversations in a prompt cache in the node's memory, so that the next turn of a
  conversation is answered faster. Our Linux installer turns it on. The cache has a size limit but no time limit: a
  conversation stays there until newer ones push it out or the node restarts.
- We record which node answered each request, never the conversation itself, so a reported leak can be traced and
  the volunteer banned.
- Node operators agree not to log, keep or share conversations, and to use them only to compute your answer
  (our [terms for operators](/about/terms/#6-requests-your-node-answers)). Breaking that means a permanent ban: all
  their nodes cut off at once, their credit removed and, for BOINC volunteers, their project account closed. We
  publish a short note about every confirmed case.
- A volunteer's node answers chats only after its operator has accepted those terms; until then it does science work
  only.
- We, who run the gateway, could see messages while they pass through. The gateway does not store them, apart from the
  evidence of a safety check (below).

If you need confidentiality, run a model on your own machine. AIs@home's software is open source
([repository](https://github.com/aisathome/aisathome)), so you can.

## Where nodes are

Answers can be computed on volunteers' machines anywhere in the world, including countries where the GDPR does not
protect what you write. That is why users may not put personal data into chats. If you need a GDPR-compliant or
EU-hosted service for personal data, use an established provider. We send nodes no account, name, email address or IP
address. Messages that look like personal data (an email address, a payment card number, an IBAN or a Norwegian
national identity number) go only to our own nodes, but detection is incomplete: names, addresses and health details
are not detected. Hardened, not confidential: the volunteer whose machine answers can read the conversation.

The nodes we run ourselves, including rented ones, are only in the EEA or a country with an EU adequacy decision.

## What we store, and why

**Your account.** GitHub's numeric user id or Google's account identifier ("sub"), depending on which you sign in with;
its state (active, waiting or banned) and when it was created. No name and no password. API keys and node tokens are
stored only as a hash.

**Chats.** Temporary by default, and then not stored. Chats you save are kept in the chat app on our gateway server
until you delete them or your account.

**For each request:** time, a pseudonymous user id, which node answered, the model, token counts, duration, the credit
it earned, and whether a safety check flagged the answer (which rule, never the content). Never the text.

**Evidence of a safety check.** When our safety checks stop or flag a node's answer, we keep the part of that answer that
triggered the check (at most 300 characters, with known secret formats removed) for 14 days, so we can tell a real
attack from a false alarm. It is the node's text, but it can quote something from your conversation.

**If you run a node:** your node tokens (as a hash), the names you give your nodes, what they served and the credit they
earn; whether you have accepted the operator terms, and when.

**Your username, if you set one, is public**, together with your credit: on the [leaderboard](/network/) and in our
statistics files in BOINC's format, which other sites can copy. Without a username you appear in totals only.

**Your choices:** whether your API keys allow volunteer nodes for agents.

**Email about important changes (only if you ask).** If you tick "Email me about important changes and outages" when
you sign in, or turn it on on your account page, we store an email address with your account: the one GitHub or Google
marks as verified (we ask them for it only then). We also store when you agreed and where the address came from. We use
it only to write to you ourselves, from contact@aisathome.org, about important changes to the service and outages: no
newsletters, no marketing. It is never shown publicly, never sent to nodes and not written to our logs. The mail goes
out through Amazon SES (Amazon Web Services, Stockholm region). If an address bounces or marks our mail as spam, SES
keeps it on its suppression list. Turn it off on your account page at any time and the address is deleted at once; we
also delete it if you ask us or if we ban your account.

**Your IP address** is used in memory for rate limits and to limit nodes per address, and for a minute or so for the
visitor statistics below; we do not store it. Our servers' process logs hold no message text and no IP addresses.

**In memory only:** to keep a conversation on the same model file, the gateway remembers the length and hashes of each
answered conversation and which model answered, never its text, for 24 hours after the last answer. To reuse a node's
cache, it keeps hashes of text blocks per node for 10 minutes.

**Visitor statistics (this website only).** To know whether anyone reads the site, the server counts visits to its
pages from its own web server log: no cookies, no JavaScript, no outside service. The chat, the API and the account
pages are not counted.

- The log lines, which contain your IP address and your browser's name (user agent), are kept only in the server's
  memory, never on disk or in backups. About once a minute they are counted and deleted.
- What we keep, per day: views per page (without anything after a `?`), the domain of the site that linked here (for
  example `reddit.com`, never the full address), also per page, how many 404s, how many requests came from known bots
  and from our own tests, and an estimate of unique visitors.
- For that estimate the server makes a hash of your IP address and browser name together with a random value that
  exists only in memory and is replaced at the end of each day (UTC). Only the day's count is kept, so visits on
  different days cannot be linked, not even by us.
- The code that does the counting is public:
  [`tools/sitestats`](https://github.com/aisathome/aisathome/tree/main/tools/sitestats).

**Cookies.** A sign-in cookie (only once you sign in), `aisathome_theme`, which remembers whether you chose light or
dark, and `aisathome_notice_…`, which remembers that you dismissed a notice on the account pages (only the notice's id).
The chat remembers a dismissed notice in your browser's own storage instead. No tracking or advertising cookies.

## Our BOINC project (boinc.aisathome.org)

The BOINC project runs BOINC's standard server software on its own server, also in Stockholm (Amazon Web Services,
eu-north-1). It stores what every BOINC project stores:

- **Your BOINC account:** an email address, a hash of your password, your screen name, your country if you give one,
  your team, your account keys, your preferences, and your forum posts and profile if you write any.
- **Your computers:** what the BOINC client reports to every project (processor, GPU, memory, operating system, BOINC
  version, the computer's name, its time zone) and its IP addresses.
- **Your tasks:** which tasks your computers ran, their results and the credit they earned. A task's log goes to our
  server with the result; it holds no one's chat.
- **Public:** your screen name, credit, team, country if you give one, and your forum posts, as on every BOINC
  project. Your computers are listed publicly only if your BOINC preferences allow it.

**Statistics exports and your CPID.** Like every BOINC project, we publish daily statistics files (users, teams,
computers) that statistics sites and Gridcoin copy. The user file has your screen name, credit, team and a
cross-project id (CPID), a hash made from a random id and your email address. With your own email address, the same
CPID appears on every project you use, so statistics sites can add up your credit; with a made-up nomail address it is
ours only. We publish these files because volunteers want their credit counted across projects (our legitimate
interest, GDPR Art. 6(1)(f)). If you don't want that, switch your BOINC account to a made-up address, or write to us.
Copies already taken by statistics sites are theirs; after you delete your account, our files list it as deleted.

**Our server logs** on the BOINC server: web access logs and error logs, kept 14 days. While the project is
invitation-only, the error log also records the email address of each new account next to its invitation code.

## Legal basis

| What | Why we may (GDPR Art. 6(1)) |
|---|---|
| Chat and API: passing your messages to a node, saved chats | (b) contract: it is the service you ask for |
| Account, API keys, node tokens, usage limits and credit | (b) contract; (f) legitimate interest in preventing abuse |
| Opt-in email about changes and outages | (a) your consent, which you can withdraw at any time |
| BOINC account, computers, tasks and IP addresses on the BOINC server | (b) contract: taking part in the project |
| BOINC statistics exports with CPIDs | (f) legitimate interest: credit counted across projects, as BOINC volunteers expect |
| Server logs on both servers | (f) legitimate interest: security and running the service |
| Evidence of a safety check (part of a node's stopped or flagged answer, 14 days) | (f) legitimate interest: security, protecting users from malicious nodes and telling a real attack from a false alarm; limited to at most 300 characters, with known secret formats removed, for 14 days |
| Visitor statistics (counts) | (f) legitimate interest: knowing whether anyone reads the site |

You need a GitHub or Google account id to sign in, and an email address for a BOINC account; without them we cannot give
you an account.

**Personal data in chats.** Our terms forbid putting personal data into chats, and we don't need any to answer you. If
you do it anyway, we still handle it the way this page describes: the gateway passes it to a node without storing it,
messages that look like personal data go only to our own nodes, and the operator's terms forbid keeping or using it.
If a node fails before it answers, the conversation goes to another node; when detection missed the personal data, that
can be another volunteer's.
That is still processing of your data, under the same basis as the rest of the chat (contract). It applies just as much
to health details, religion, politics or other sensitive information, which you should never type in.

Automatic checks can stop an answer or hold a node until a person looks (for example a dangerous command in an answer).
A hold is temporary. Bans, of users or of node operators, are decided by a person, never by a machine alone.

## Who else gets data

- **Node operators:** the conversation text, as described above, under terms that forbid keeping or using it for
  anything but your answer.
- **Amazon Web Services** hosts both servers, our backups and our email sending, in Stockholm (eu-north-1), under its
  data processing terms.
- **GitHub or Google** when you choose to sign in with them; they tell us your user id (and your verified email address
  only if you ask for email notices).
- **Statistics sites and Gridcoin** copy the public BOINC statistics files.
- Nobody else, unless the law requires it.

## How long we keep it

| Data | How long |
|---|---|
| Message text on the gateway | not stored, apart from the evidence of a safety check |
| Evidence of a safety check (part of a node's stopped or flagged answer) | 14 days |
| Message text on a node | used only to compute the answer (operator terms); recent conversations stay in the node's memory cache until newer ones push them out or the node restarts (no time limit) |
| Temporary chats | not stored |
| Saved chats | until you delete them or your account |
| Account, choices, usage and pilot records | as long as your account exists |
| Opt-in email address | until you turn it off, we ban the account, or you delete it |
| Server process logs (gateway) | 14 days, no message text, no IP addresses |
| BOINC account, computers and forum posts | until you delete your account |
| BOINC web access and error logs | 14 days |
| BOINC public statistics files | as long as your BOINC account exists; then it is listed as deleted |
| Backups (both servers) | 7 days |
| Visitor statistics | daily counts only; raw log lines about a minute, in memory |

When we delete something, copies in our backups disappear within 7 days.

## Your rights

You can ask us to:

- tell you what we hold about you and give you a copy (Art. 15, 20);
- correct it (Art. 16);
- delete it (Art. 17);
- stop or limit using it (Art. 18), or object to uses based on our legitimate interest, such as the statistics exports
  (Art. 21);
- withdraw your consent to email notices at any time; that does not affect what happened before.

Write to contact@aisathome.org from the address you want the answer at. Before we give out or delete anything, you
show that the account is yours: by signing in to your aisathome.org account or, if you only have a BOINC account, with
your full BOINC account key. We never accept the weak account key as proof, because people share it with others who
compute for them. We answer within one month.

**Deleting your account.** Write to contact@aisathome.org and we delete your aisathome.org account, your saved chats,
your API keys and node tokens, your usage and pilot records, your email address if you gave one, and your BOINC account,
within one month. Backups age out within 7 days. We cannot delete copies that statistics sites took of the public BOINC
files, or anything a node operator kept in breach of our terms.

**Complaints.** You can complain to the Norwegian Data Protection Authority, Datatilsynet (datatilsynet.no), or to the
authority where you live. We'd like to hear from you first.

## Age

You must be 18 or older to use AIs@home, its BOINC project, or to run a node.

## Limits

The number of accounts is limited while the network is small (new accounts wait in line), and each account has a daily
usage limit.

## Changes

When we change this page, we change the date at the top. For bigger changes we also post a notice on the site and, if
you asked for email notices, write to you.
