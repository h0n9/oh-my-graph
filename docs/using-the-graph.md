# Use the graph

[Back to Quick Start](../README.md#quick-start)

## Explore and manage topics

Open **`http://localhost:7780/`** in your browser to explore the graph visually — the topic list shows node/edge counts, and `/graph?topic=<name>` renders a live interactive force-directed graph. Selecting a node opens its details, including any tags stored on that node.

Use the **pencil icon** to rename a topic or the **archive box icon** to archive it. Archiving preserves the graph and reserves its name while hiding the topic from **Topics**. Open **Archived** and use the **restore arrow** to make a topic active again or the **trash icon** to permanently delete its graph data. After deletion, a new topic can use the same name.

## Manage connected agents

With [`--auth`](setup.md#security-and-authentication), the topic list header shows an **N agents** link (N counts agents with a live session). It opens **`/agents`**, a page of every AI client that has registered with your server. Without `--auth`, the page does not exist.

Each card shows:

- **Status**: `active`, `idle Nd` (unused for over 24 hours), `expired`, or `no tokens` (registered but holds no tokens, for example it never finished authorizing).
- **Connected**, **last used**, and **session expires**. Dates are UTC. *unknown* means the server has no record yet, for older connections or agents that never got tokens.

What you can do:

- **Revoke** cuts an agent off right away, after you confirm in a dialog. Its tokens and pending codes stop working and its registration is removed. A request already in progress finishes; every later request is rejected.
- **Remove** is the same for agents with no live session, with the same dialog.
- **Prune inactive agents** removes every inactive agent older than an hour. It never touches an agent that is mid-authorization. It runs only when you click it, and it does not ask first.

After a revoke, the agent must authorize again with your passphrase. Some clients cannot recover on their own, so you may need to remove and re-add the connector. You may also be revoking the agent you are using right now.

After a server restart, *last used* can be up to an hour behind.

## Continue work across assistants

1. **Capture an idea with ChatGPT Voice.** Save the problem, findings, and decisions to a project topic.
2. **Implement with Claude Code.** Ask it to read the topic and use the saved decisions to propose and implement the change.
3. **Prepare documentation with another assistant.** Ask it to read the same topic for the original goals and implementation details.

## Messaging

Agents communicate asynchronously via `message` nodes in a shared topic:

1. **Session A** writes a `message` node to topic `"comms"`
2. **Session B** polls `read_nodes_since_cursor("comms", cursor: last_cursor, types:["message"])` and sees the message
3. **Session B** replies with a new `message` node + `replies_to` edge pointing back

No extra infrastructure needed — the graph is the message bus.
