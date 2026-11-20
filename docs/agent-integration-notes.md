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

- A reply whose card is closed (paid, rejected, confirmed) still offers **Regenerate**; regenerating would ask the agent for another card.
- A card answered from another tab gives the first tab a 409, and that tab shows the old card until it is reloaded.
- A Web UI that runs against the real BFF needs the token program on its own computer; there is no static token setting, on purpose.
## Decisions the tests pinned and did not change

Edge-case tests found these, and they are choices more than bugs, so they stay as they are until someone decides:

- **A 2xx answer to a card's action whose body is empty or unreadable is an error** ("reading the BFF's answer"), even though the action may
  have gone through at the service. After a payment, how to show a success that cannot be told is a product decision.
- **A zero, negative or absurd amount on a transfer card** offers Reject only; the service has the last word on whether it can be paid.
- **`AGENT_REQUEST_TIMEOUT` has no floor or ceiling** (`1ns` makes every call time out; `999h` is accepted).
- **A refusal on a request without htmx** redirects to the conversation and loses its reason; only the htmx answer shows it on the card.
- **A graceful close of Chrome DevTools MCP** does not kill its process group, so a Chrome helper that outlives the server would survive it.
- **A secret split across two lines** is not found by the explorer's filter. The accessibility tree is one element a line, and the password is
  only typed in the sign-in page, from which nothing is recorded.
- Several answers to one card may each reach the service (the gateway is called before the compare-and-set save): exactly one save wins, the
  others get `ErrInvalidState`, but the service's own idempotency is what keeps a double click from paying twice.
