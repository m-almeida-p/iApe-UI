# The conversation id: who makes it, and what happens to it on the way

One conversation has to keep **one id from the browser to the history**. For a chat that iApe-UI started before this change it did not:
the id the UI sent was rewritten on the way. This page explains why that matters, what each piece does with the id, and the fix.

## The pieces and their ids

```
iApe-UI ──threadId──▶ agui-bff ──contextId──▶ agent (ms-ml-genai-agents-bs) ──▶ event_store ──▶ Kafka ──▶ consumer ──▶ history ──▶ ms-ml-conv-history-bs
 (makes it)           (passes it, or derives one)  (requires conv_v1_<uuid v4>)        conversation_id                                   _id of a conversation
```

| Piece | What it does with the id |
|---|---|
| **iApe-UI** | Makes the id when a chat starts (`domain.NewConversationID()`: `conv_v1_` + a UUID v4) and sends it as `threadId` in every `POST /agent`. Before, it sent the `web_ui` conversation id, a UUID v7. |
| **agui-bff** (`ms-ml-genai-agents-bs/local/agui-bff`) | `contextIdFor(threadId)`: a `threadId` that already is `conv_v1_<uuid v4>` is the `contextId`, unchanged (as production does, and as the BFF's own page sends since PR #509). **Any other `threadId` is derived**: `conv_v1_` + a UUID v4 built from `sha256("thread:" + threadId)`. One way: from the result you cannot get the `threadId` back. |
| **The agent** | Accepts only a `contextId` of the form `conv_v1_` + a lowercase UUID v4 (44 characters). Anything else is a legacy id (the account's own) or invalid (`yapeauth/conversationid.go`). It reloads the conversation from its events when the same `contextId` comes again. |
| **Event store, Kafka, consumer** | Carry that `contextId` as `conversation_id`, untouched. |
| **History API** | Lists and reads conversations by that same `conv_v1_…` id. |

The production BFF does not rewrite anything: its `conversationSession` query mints `conv_v1_<random uuid v4>` and uses it as thread,
session and context id. The local `agui-bff` was a harness that had no such query, so it derived the id from the thread; it now passes
through a thread id that already has the agent's shape and derives one only for any other.

## The problem

1. **For a `threadId` that does not have the agent's shape, the id that reaches the history is not the one the UI made.** iApe-UI
   sent a UUID v7, so its `threadId` and the history's `conversation_id` were two different strings for the same chat. Looking for
   `conv_v1_<ui id>` in the history finds nothing.
2. **They can be matched only by recomputing the hash**, which only works for chats the UI itself started. Checked on the local data on
   2026-11-20: of 95 UI chats, 91 had their hash in the history (the other 4 never reached the agent); the history held 12 more
   conversations that no UI chat explains (demos and other channels).
3. **A conversation whose id was derived cannot be continued by its history id.** To continue one, the UI must send a `threadId`
   that becomes that conversation's `contextId`; for a derived id the only such `threadId` is the one it was derived from, which the
   history does not know. A `threadId` that is the history's own `conv_v1_…` id is passed through, so such a conversation is continued
   by sending its id.
4. **The UI keeps its own copy of every chat** (`web_ui.conversations` and `messages`) next to the history: two sources for the same
   conversation, joined only by a hash.

## The fix

- **The UI makes the id, once, when a chat starts**: `conv_v1_` + a lowercase UUID v4, exactly the shape the agent requires. It sends
  it as `threadId` on every request of that chat.
- **The BFF passes it through.** In `contextIdFor`: if `threadId` is already a valid `conv_v1_<uuid v4>`, return it as it is; otherwise
  keep the hash (so threads made before still resolve). This is what production does; the PoC branch of the agent
  (`feat/AIP-472/poc-tx-and-projections-v2`) has it, with its tests.
- **The id that the UI made is then the id in the history.** The history is the only source of chats; the UI reads them from there and
  needs no copy.

## How to check an id end to end

```bash
# the conversations the history has for the account (first page)
curl -s "localhost:3100/v1/conversations?idYapeAccount=$YAPE_ACCOUNT_ID&page[size]=5"
# does a given conversation exist, and what did it hold
curl -s "localhost:3100/v1/conversations/<conv_v1_...>/messages"
```

After the fix, start a chat in the UI, send one message, and the id in the page's requests (`threadId`) must be the `_id` of a
conversation in that list within a few seconds (Debezium, Kafka and the consumer are asynchronous).
