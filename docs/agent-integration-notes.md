# Talking to the agent service: what was found

What building the Web UI against the Yape agent service's BFF taught us, and that the code or its tests could not say. Each entry
says how it was seen and what the code does about it. Nothing here is a secret: the one password that appears is the fixed
development password of the cluster's Redis, which is in that repository's own `docker-compose.yml`.

## The protocol

| Finding | What the code does |
|---|---|
| `POST /agent` answers **201**, not 200 (a NestJS POST). `curl` does not show it. The first live run failed on it | The client accepts any 2xx, and checks `Content-Type: text/event-stream` |
| `RUN_FINISHED` can arrive **before the last `TEXT_MESSAGE_END`** | The decoder reads to the end of the stream, not to `RUN_FINISHED`, so it keeps the last text |
| A stream cut after some text is an answer that stopped short; one cut before any is an agent that did not answer | `ErrIncompleteStream` (the text is kept, with a Retry) and `ErrAgentUnavailable` |
| After approve, reject or the code, **the BFF itself** tells the agent (`notifyPaymentCardSync`). That notice is not in the stream the page reads | Nothing: the page shows the `displayMessage` of the action |
| An action that is refused answers 4xx with the reason in `message` (a string, or a list) | `*domain.ActionError`: the card stays as it was and shows the reason |
| The approve response asks for a code with `otp_required` and `transfer_id`; the code is confirmed against that `transfer_id` | The card keeps it as `ChallengeID`. `otp/confirm` without it is a 400 |
| The stream carries **no token counts** | The page shows the times and no tokens (`data-prompt-tokens` is empty) |
| A 401 means the token expired | The token program is asked again with `--refresh`, once |

## The cards and the mock bank

- **Over 500 PEN** the agent refuses on its own (`validate_p2p_amount`), before any card. **Over 100 PEN**, or a message with `[otp]`,
  needs a code; anything else is paid at once.
- **The mock bank accepts any code** (`000000` paid). The path of a wrong code is tested with the scripted BFF (`internal/testbff`),
  not live.
- **The BFF remembers the contacts that were confirmed**, per account. A name that was used once never asks for confirmation again, so
  a test of the contact card needs a name that is new on each run (the explorer makes one).
- The picker of the BFF's own page sends the **chosen contact's name** as `query` to `POST /bff/contacts/confirm`, not what was typed
  after the `@`: with only an `@` the query is empty and the BFF answers `Missing required fields: query, phone`.
- The destination's name on the transfer card comes from the agent's mock (it said "Rosa Diaz", then "Usuario"), whoever you chose. The
  phone is the chosen contact's.
- The candidates of a contact card are made up by the Web UI (`domain.Candidates`), as the BFF's page does. There is no address book.
- Asking for a contact with a name nobody made up is not steady: an LLM sometimes does not take a bare invented name as the payee.
  "quiero yapearle 20 soles a <name>" is.

## The guardrail and the ban

The agent service's prompt guard counts a blocked message against the account (a circuit breaker per account, in Redis). In a development
`.env` of the service it is set hard:

```
CIRCUIT_BREAKER_MAX_BLOCKS_PER_WINDOW=2        # (default 10)
CIRCUIT_BREAKER_MAX_WINDOW_SECONDS=300
CIRCUIT_BREAKER_BAN_DURATION_SECONDS=2600000   # about 30 days (default 300)
```

So **two blocked messages in five minutes ban the account for a month**, and every message then answers *"Por seguridad, limitamos tu
acceso al asistente. Podrás intentarlo de nuevo más adelante."* That is what a run of the explorer's `guard` path (a prompt injection) does,
and it happened: the next paths all failed with that text, which looks like a bug of the page and is not.

- The explorer's `-all` leaves out `guard` unless `-with-guard` is given, and running it says what it costs.
- To lift a ban, delete its key in the cluster's Redis (`requirepass` is set; the development password is the one in
  `cluster/docker-compose.yml` of the Yape repositories). The account is the one in `YAPE_ACCOUNT_ID`:

  ```sh
  docker exec -e REDISCLI_AUTH='<that password>' yape-redis redis-cli TTL promptguard:cb:banned:<account>   # seconds left; -2 if none
  docker exec -e REDISCLI_AUTH='<that password>' yape-redis redis-cli DEL promptguard:cb:banned:<account> promptguard:cb:blocks:<account>
  ```

- For tests, soften the service's settings (50 blocks, 60 s of ban, say) rather than lifting the ban each time.

## The environment

- The **QA AI gateway** sometimes answers 503 or 429. The agent then serves its fallback text, and a run of the flows looks broken for
  that reason alone (the service's log says `api_error_status_503/429`). Wait and run again.
- `yape-token` needs the VPN and its own configuration. It prints the token to stdout only, caches it (mode 0600) and takes `--account`
  and `--refresh`. The Web UI never writes, logs or returns the token.
- **A container cannot run `yape-token`**: it belongs to the computer the Web UI runs on. So Compose runs only PostgreSQL and the
  scripted BFF, and the container test copies `test-bff` into the image as its token program (`test-bff --account ID` prints a token).
- The **explorer's Chrome** starts through Chrome DevTools MCP, with a window by default (a fresh, isolated profile: it is not your
  Chrome). `-headless` runs it without one. Either way `--isolated` and the flags that keep usage statistics off are required. `npx`
  needs a recent Node (20.19+, 22.12+ or 23+).

## What the Web UI does not do yet

- The cards are not kept: a reload makes them disappear, and so does a restart of the server (see *One answer per card*).
- A card answered from another tab gives the first tab a 409, and that tab shows the old card until it is reloaded.
- A Web UI that runs against the real BFF needs the token program on its own computer; there is no static token setting, on purpose.
## Decisions the tests pinned and did not change

Edge-case tests found these, and they are choices more than bugs, so they stay as they are until someone decides:

- **A 2xx answer to a card's action whose body is empty or unreadable is an error** ("reading the BFF's answer"), even though the action may
  have gone through at the service. After a payment, how to show a success that cannot be told is a product decision.
- **A zero, negative or absurd amount on a transfer card** offers Reject only; the service has the last word on whether it can be paid.
- **`AGENT_REQUEST_TIMEOUT` has no floor or ceiling** (`1ns` makes every call time out; `999h` is accepted). A turn of the agent keeps it as it is; an answer to a card is cut to 60 s: a person is waiting for a call to the payment service.
- **A refusal on a request without htmx** redirects to the conversation and loses its reason; only the htmx answer shows it on the card.
- **A graceful close of Chrome DevTools MCP** does not kill its process group, so a Chrome helper that outlives the server would survive it.
- **A secret split across two lines** is not found by the explorer's filter. The accessibility tree is one element a line, and the password is
  only typed in the sign-in page, from which nothing is recorded.

## One answer per card (the service decides)

The agent's service is reached from phones too, where retries and double taps are normal. The Web UI used to **claim** a card (save it as
`processing`, with a compare-and-set) before asking the service, so that only one answer reached it. That was removed with the chats'
storage: the cards now live only in the page's reply, in this server's memory, and **the service is what keeps one transfer from being
paid twice**. It ignores a repeat of an answer, which was checked against the real BFF: a second `approve` of the same approval is `400
{"error":"approval already resolved: APPROVED"}` (also a third), not a second payment.

What the card does with each answer:

| The service... | The card |
|---|---|
| acted | kept in its new state (`paid`, `rejected`, `otp_required`, `confirmed`) |
| said no (`ActionError`), could not be reached, timed out, or did not confirm the transfer | **as it was**, with the reason, and it can be answered again: that is safe, because the service decides |
| says the approval was answered already (`approval already resolved: APPROVED`/`REJECTED`) | the card becomes that: `paid` ("This transfer was already approved. Its result is in your history.") or `rejected` |

Several answers at once (a double tap, a second tab): those that find the card open all reach the service, which does one and says
`already resolved` to the others; those that come after the card closed get `ErrInvalidState` and the page is given the reply as it is now.
The card ends as what was done, and the first answer to be kept is the one that stays (a compare-and-set on the card's state, in memory,
only to keep the first). The page keeps the button off while the request is out (`hx-disabled-elt`). Eight simultaneous answers are one
payment: `internal/core/interaction`. Against the real BFF: `it/live` (`TestLiveFlow`, a second click shows the same state).

The BFF explains a refusal in `message` (NestJS) or in `error` (the approvals' own); the client reads both (an `error` that is only the
status name, as in `"Bad Request"`, explains nothing) and sets `ActionError.Resolved` to the word after `already resolved:`. Any other
word (`EXPIRED`), or an approval that cannot become that (a rejection of a card that waits for a code, whose approval is `APPROVED`), is
given back with the reason.

Caveat: `APPROVED` does not say whether the transfer was paid or still waits for its code (the first reply, with the transfer id the code
needs, is the one that was lost), so the card says "approved" and sends the user to the history for the result.

What is lost: a reload makes the card disappear (the history keeps no cards: that is the module `card-events`, later), and a restart
forgets the reply and its card. A payment made is in the user's bank history either way.

### What the review added (phase 5)

- **Losing the race to answer a card does not wipe the reply.** htmx swaps a 409 over the whole reply, so the page is given the reply as it
  is now; if it cannot be read, the 409 carries `HX-Reswap: none`. 
- **A 2xx that does not confirm the transfer is not paid.** An answer to approve or the code with `success: false`, or an `execution.status`
  that is not `SUCCESS`, is an answer that may or may not have gone through: the card is as it was, and answering again is safe. A BFF that says nothing about
  success (a reject, an older BFF) is taken at its word.
- **The card routes are limited per user** (`approve`, `reject`, `otp` and `/contacts/confirm`, in a bucket of their own, apart from the
  messages'): each is a call to the payment service and runs the token program. There is **no cap of wrong codes** on a card: the bank owns
  its attempts, and the mock bank accepts any code.
- **A poll that cannot read its card stops** (status 286, for which `base.html` swaps nothing). The picker confirms only the person shown
  (`name` and the query the list was drawn for are posted; the server checks them). The client does not follow redirects, and the token
  program does not get `DB_URL`, `DATABASE_URL`, `ADMIN_PASSWORD` or `POSTGRES_PASSWORD`. The explorer's `report.md` and failure text go
  through the same secret filter as its records.
