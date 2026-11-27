---
section: Fixed
---

- **A request for a model a connector's pilots serve waits for a pilot instead of failing.** The
  gateway answered 404 "not served on this network" at once for any model no node had offered in
  the last 24 hours, so after a restart, or a day with no such node online, the first science
  request could never wait for the pilot its demand would bring. A connector's new `models` lists
  the names and roles its pilots serve; a request for one of them waits up to the queue timeout
  while the connector may create pilots and they may serve that kind of request. Other unknown
  models still get 404 at once. (gateway)
