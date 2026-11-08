// Command aisathome-sitestats counts visits to the website from the web server's access log,
// without cookies, without JavaScript and without any third-party service. This file is the
// complete description of what is counted; the code next to it does exactly this.
//
// Input: Caddy's JSON access log for the website host only (not the API, the chat or the
// account pages; the deployment skips those paths in Caddy, and this command ignores them
// again). The log is written to a tmpfs (memory) and never to disk. Every run (once a
// minute) reads the new lines, adds them to the day's counts and then frees the lines it
// read (on Linux the file region is punched out; the memory is released at once). Raw
// lines, with addresses and browser names, therefore live for about a minute, in memory only.
//
// Output (a small JSON file, kept): per UTC day
//
//   - page_views: page loads (GET, answered 200 or 304, path ending in "/" or ".html") by
//     browsers that are not known bots;
//   - pages: the same per path, with the query string removed (only paths of pages that exist:
//     a 404 is never stored by path);
//   - referrers: the referring site's domain only (for example "reddit.com"), never the full
//     address, its path or query; our own domains are not counted;
//   - page_referrers: the same referring domains per page (for example: 3 views of
//     "/science/tauceti/" came from "reddit.com"), counted only with the page views above, so
//     never for a 404, a bot or our tests. While the UTC day runs, up to 100 domains per page
//     are counted exactly (new ones beyond that go under "(other)"); once the day is over,
//     the 20 with the most views that day keep their names and the rest are added together
//     under "(other)";
//   - visitors: an estimate of unique visitors, see below;
//   - feed: requests for the RSS feed (/devlog/rss.xml);
//   - not_found: answers 404, as one number;
//   - bots: requests from known bots and scripts (the list in stats.go, an empty user
//     agent, or one that does not start with "Mozilla/"), by a fixed bot name from that list
//     ("other" for the rest); they never count as page views or visitors;
//   - tests: requests from the project's own automated tests (user agent containing
//     "aisathome-test" or "aisathome-smoke"), only counted, nothing else.
//
// Unique visitors: for each human page view, SHA-256(salt + IP address + user agent), where
// salt is 32 random bytes made for the current UTC day. The hashes of the day are kept in the
// state file on the tmpfs, only to count how many differ; the count goes into the output. At
// the end of each UTC day the salt and the hashes are deleted, so visits on different days
// cannot be linked, not even by us. If the server restarts during a day, the salt is lost and
// visitors may be counted twice that day: it is an estimate.
//
// Never in the output: IP addresses, user agents, hashes, salts, full referrer addresses,
// query strings, cookies.
//
// Usage:
//
//	aisathome-sitestats run -host aisathome.org -log /run/sitestats/log/access.log \
//	    -state /run/sitestats/state -out /var/lib/aisathome/sitestats/stats.json
//	aisathome-sitestats report [-days 7] [-file stats.json] [-page /science/tauceti/]
//
// SPDX-License-Identifier: Apache-2.0
package main
