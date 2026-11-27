---
section: Fixed
---

- **The pressure statistics count every node science may use.** Science's slots and demand in
  `/v1/stats/pressure` left out pilots that serve science only and nodes without an account that
  the model check admitted, though science requests run on both; the counts were too low wherever
  such nodes were online. They now judge a project's nodes by the same rules as its requests and
  the connectors' pilot demand. (gateway)
