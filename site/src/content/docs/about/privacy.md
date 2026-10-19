---
title: Privacy
description: How AIs@home handles your messages.
---

AIs@home is an experiment: chat with an open AI model that runs on volunteers' GPUs instead
of a big company's data centre. Read this before you type anything you care about.

**Where your message goes.** Your message travels encrypted to our gateway (a server in
Stockholm), which passes it to a volunteer's computer (a "node"). The node runs the model
and sends the answer back the same way.

**Who can see it.**
- **The person running the node** that answers you can technically read the conversation
  their computer processes. We cannot prevent that: home GPUs have no way to hide what they
  compute. Any volunteer with a suitable GPU can run a node, so assume a stranger may read
  your chat. Nodes never learn who you are: no name, no email, no account id.
- **Messages that look like personal data** go only to the maintainer's own nodes
  ("trusted nodes"). Trusted means run by us, not secure: some of them are rented machines,
  whose host could in principle see what passes through.
- **Agents** (requests with tools, for example opencode) go to volunteers' nodes only if
  your API key allows it; otherwise only to your own node or the maintainer's.
- **We, who run the gateway,** could see messages while they pass through. The gateway does
  not store them.

**What we store.**
- Your account: GitHub’s numeric user id or Google’s account identifier (“sub”), depending
  on which you sign in with; its state (active or waiting) and when it was created. No
  email, no name, no password: we ask Google only to confirm who you are (the “openid”
  scope). API keys are stored only as a hash.
- Chats: temporary by default. Chats you save are kept until you delete them.
- For each request: time, a pseudonymous user id, which node answered, token counts,
  duration, the credit it earned, and whether a safety check flagged the answer (which
  rule, never the content). Never the content. Kept as long as your account exists.
- If you run a node: your node tokens (as a hash), the names you give your nodes, and the
  credit they earn.
- **Your username, if you set one, is public**, together with your credit: on the
  [leaderboard](/network/) and in our statistics files in BOINC's format, which other sites
  can copy. Without a username you appear in totals only.
- Your IP address is used in memory for rate limits and to limit nodes per address, and
  for a minute or so for the visitor statistics below; we do not store it.

**Visitor statistics (this website only).** To know whether anyone reads the site, the
server counts visits to its pages from its own web server log: no cookies, no JavaScript,
no outside service. The chat, the API and the account pages are not counted.
- The log lines, which contain your IP address and your browser's name (user agent), are
  kept only in the server's memory, never on disk or in backups. About once a minute they
  are counted and deleted.
- What we keep, per day: views per page (without anything after a `?`), the domain of the
  site that linked here (for example `reddit.com`, never the full address), how many 404s,
  how many requests came from known bots and from our own tests, and an estimate of unique
  visitors.
- For that estimate the server makes a hash of your IP address and browser name together
  with a random value that exists only in memory and is replaced at the end of each day
  (UTC). Only the day's count is kept, so visits on different days cannot be linked, not
  even by us.
- No IP addresses, hashes or browser names are stored. The code that does the counting is
  public: [`tools/sitestats`](https://github.com/aisathome/aisathome/tree/main/tools/sitestats).

**Cookies.** A sign-in cookie (only once you sign in), and `aisathome_theme`, which remembers
whether you chose light or dark. No tracking or advertising cookies.

**What we never do.** We don't sell data, don't show ads, don't train models on your
chats, and run no tracking: no analytics scripts, no outside analytics service, no
tracking cookies. The visitor statistics above are counts, made on our own server.

**A safety net, not a guarantee.** We automatically hide well-known secret formats (API
keys, private keys) from nodes, and messages containing things like email addresses or
card and account numbers only go to trusted nodes. Answers from volunteers' nodes are
checked for dangerous commands and unknown web addresses, which are stopped or flagged.
Detection is incomplete in both directions. **Do not paste passwords, secrets or other people's personal data.** You can see the code for this in the [repository](https://github.com/aisathome/aisathome).

**This service is not confidential.** It is hardened, not private. If you need
confidentiality, run a model on your own machine. AIs@home's software is open source, so
you can.

**Limits.** The number of accounts is limited while the network is small (new accounts wait
in line), and each account has a daily usage limit.
