---
title: cocoonbox
---

# cocoonbox

A persistent cloud computer per user, with a resident agent that any model can drive and a native macOS client to watch it, talk to it, and take over. Local coding agents (Claude Code, Codex, Cursor) use the same box through MCP. Boxes are [cocoon](https://cocoonstack.github.io/cocoon/) microVMs claimed from [sandbox](https://cocoonstack.github.io/sandbox/) warm pools; model calls go through [gateway](https://cocoonstack.github.io/gateway/).

## Guides

- [Design](design.md): goals, stack, the box, the agent, the app, state, identity, sharing, Connect, offload and the cost model
- [Contracts](contracts.md): the gRPC services, MCP surfaces, the toolhost wire and the desk wire every component implements
- [Boundaries](boundaries.md): the layering each component follows and what crosses each boundary
- [Measurements and lessons](bench.md): clone, wake, hibernate, restore, VNC and image-cut measurements on the bench host
- [Sandbox backend evaluation](m45-sandbox-eval.md), [cua-driver spike](cua-driver-spike.md), [Connect research](connect-research.md)

Source: [github.com/loomhand/cocoonbox](https://github.com/loomhand/cocoonbox)
