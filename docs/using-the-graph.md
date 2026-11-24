# Use the graph

[Back to Quick Start](../README.md#quick-start)

## Explore and manage topics

Open **`http://localhost:7780/`** in your browser to explore the graph visually — the topic list shows node/edge counts, and `/graph?topic=<name>` renders a live interactive force-directed graph. Selecting a node opens its details, including any tags stored on that node.

Use the **pencil icon** to rename a topic or the **archive box icon** to archive it. Archiving preserves the graph and reserves its name while hiding the topic from **Topics**. Open **Archived** and use the **restore arrow** to make a topic active again or the **trash icon** to permanently delete its graph data. After deletion, a new topic can use the same name.

## Manage connected agents

When the server runs with [`--auth`](setup.md#security-and-authentication), the topic list header shows an **agents** link. It opens **`/agents`**, which lists every AI client that has registered with the server:

- **Status** — `active`, `idle Nd` (live session, unused for more than 24 hours), `expired`, or `no tokens` (registered but never finished authorizing).
- **Connected**, **last used**, and **session expires** — *unknown* for connections made before the server tracked these. After a server restart, *last used* is only approximate for about an hour.

Use **Revoke** to cut off a client right away: its tokens and pending authorization codes are invalidated and its registration is removed. A request that is already in progress finishes, and every request after it is rejected. Some clients cannot recover from an unknown registration, so you may need to remove and add that connector again, and you may be revoking the agent you are using right now. Use **Remove** for entries that have no live session, or **Prune stale registrations** to remove all such entries older than an hour. Pruning never removes a client that is still mid-authorization.

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
