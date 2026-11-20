// Agent Web UI behavior: the composer, the stop button and autoscroll.
//
// Loaded from <head> with defer. There are no inline scripts (the CSP forbids
// them). The page works as plain forms without this file; this adds comfort.
//
// The server decides what is on the page. This file reads it back: a reply is
// in flight when its message has data-status pending or streaming.
(() => {
  'use strict';

  const NEAR_BOTTOM_PX = 80; // closer than this to the bottom counts as "following"
  const MAX_ROWS = 8; // the composer grows to this many lines, then scrolls

  const $ = (selector) => document.querySelector(selector);
  const input = () => $('#composer-input');
  const form = () => $('#composer');
  const sendButton = () => $('#composer [data-testid="send"]');
  const stopButton = () => $('#composer [data-testid="stop"]');
  const scroller = () => $('#messages');
  const jumpButton = () => $('#jump-latest');

  const inFlight = () => $(
    '#messages [data-role="assistant"]:is([data-status="pending"], [data-status="streaming"])');

  const isEditable = (el) => el instanceof HTMLElement &&
    (el.isContentEditable || el.matches('input, textarea, select'));

  // --- Composer ---------------------------------------------------------------

  // autosize fits the textarea to its text, up to MAX_ROWS lines.
  function autosize() {
    const box = input();
    if (!box) return;
    const style = getComputedStyle(box);
    const line = parseFloat(style.lineHeight) || parseFloat(style.fontSize) * 1.5;
    const frame = parseFloat(style.paddingTop) + parseFloat(style.paddingBottom) +
      parseFloat(style.borderTopWidth) + parseFloat(style.borderBottomWidth);
    const max = line * MAX_ROWS + frame;
    box.style.height = 'auto';
    const wanted = box.scrollHeight + parseFloat(style.borderTopWidth) + parseFloat(style.borderBottomWidth);
    box.style.height = `${Math.min(wanted, max)}px`;
    box.style.overflowY = wanted > max ? 'auto' : 'hidden';
  }

  // Enter sends, Shift+Enter adds a line. Enter that confirms an IME candidate
  // (keyCode 229 in Safari) is left alone.
  document.addEventListener('keydown', (event) => {
    const box = input();
    if (box && event.target === box) {
      if (event.key === 'Enter' && !event.shiftKey && !event.isComposing && event.keyCode !== 229) {
        event.preventDefault();
        if (!inFlight() && box.value.trim() !== '') box.form.requestSubmit();
      }
      return;
    }
    if (event.key === '/' && !event.ctrlKey && !event.metaKey && !event.altKey && !isEditable(event.target)) {
      if (box) {
        event.preventDefault();
        box.focus();
      }
    }
  });

  document.addEventListener('input', (event) => {
    if (event.target === input()) autosize();
  });

  // After the server accepted the message, the composer starts fresh and the
  // conversation follows the new turn. A refusal (422, 409) keeps the text.
  document.body.addEventListener('htmx:beforeRequest', (event) => {
    if (event.target === form()) forceFollow = true;
  });
  document.body.addEventListener('htmx:afterRequest', (event) => {
    if (event.target !== form()) return;
    const status = event.detail.xhr ? event.detail.xhr.status : 0;
    if (status >= 200 && status < 300) {
      input().value = '';
      autosize();
    } else {
      forceFollow = false; // nothing was added, so nothing to bring into view
    }
  });

  // --- Requests that fail without an answer to show -----------------------------

  // The server explains a refusal itself (409, 422, 429 are swapped in). For a
  // dropped network, a timeout or any other error status htmx swaps nothing, so
  // without this the page would look dead. The composer's alert line is always
  // there, and screen readers hear it.
  const TROUBLE = {
    403: 'That request was blocked. Reload the page and try again.',
    404: 'That chat is gone. Reload the page.',
    413: 'That message is too big to send. Shorten it and try again.',
  };

  function say(message) {
    const line = $('#composer-error');
    if (line) line.textContent = message;
  }

  document.body.addEventListener('htmx:responseError', (event) => {
    const status = event.detail.xhr ? event.detail.xhr.status : 0;
    say(TROUBLE[status] || (status >= 500
      ? 'The server ran into a problem. Try again in a moment.'
      : "That didn't work. Reload the page and try again."));
  });
  document.body.addEventListener('htmx:sendError', () => {
    say("Couldn't reach the server. Check your connection and try again.");
  });
  document.body.addEventListener('htmx:timeout', () => {
    say('The server took too long to answer. Try again.');
  });

  // --- Streaming state --------------------------------------------------------

  let wasStreaming = false;

  // syncStreaming makes the composer match the page: while a reply is in
  // flight, Send is off and Stop is on; when it ends, the composer is ready.
  function syncStreaming() {
    const streaming = inFlight() !== null;
    const send = sendButton();
    const stop = stopButton();
    if (send) send.disabled = streaming;
    if (stop) {
      stop.hidden = !streaming;
      if (!streaming) stop.disabled = false;
    }
    const log = scroller();
    if (log) log.setAttribute('aria-busy', String(streaming));
    if (wasStreaming && !streaming) {
      announceOutcome();
      refreshHistory();
      const active = document.activeElement;
      const box = input();
      if (box && (active === document.body || (form() && form().contains(active)) || active === stop)) {
        box.focus({ preventScroll: true });
      }
    }
    wasStreaming = streaming;
  }

  // refreshHistory reloads the Historial once a reply has ended, a moment later: the conversation reaches the history through the
  // event store, Kafka and the consumer, which takes about a second. One refresh is enough: the list does not poll.
  const HISTORY_LAG_MS = 1500;
  let historyTimer = null;
  function refreshHistory() {
    const list = $('#history');
    if (!list || typeof htmx === 'undefined') return;
    window.clearTimeout(historyTimer);
    historyTimer = window.setTimeout(() => htmx.trigger(list, 'history:refresh'), HISTORY_LAG_MS);
  }

  // announce says something to screen readers, politely and once.
  function announce(message) {
    const region = $('#live-status');
    if (!region) return;
    region.textContent = '';
    window.setTimeout(() => { region.textContent = message; }, 50);
  }

  const OUTCOMES = { complete: 'Reply complete.', stopped: 'Reply stopped.', failed: 'Reply failed.' };

  // announceOutcome is called once, when a reply stops being in flight.
  function announceOutcome() {
    const replies = document.querySelectorAll('#messages [data-role="assistant"]');
    const last = replies[replies.length - 1];
    const message = last && OUTCOMES[last.dataset.status];
    if (message) announce(message);
  }

  // Stop asks the server to end the reply. The answer is the reply as it is kept,
  // which replaces the streaming one and so closes its connection.
  function stopStreaming() {
    const article = inFlight();
    const reply = article && article.closest('.reply[data-stop-url]');
    const stop = stopButton();
    if (!reply || !stop || typeof htmx === 'undefined') return;
    stop.disabled = true; // one press is enough
    // htmx settles this for every status, not only for a dropped network. If the
    // reply ended, the page hides the button; if not, the press failed, and it can
    // be tried again.
    const settled = () => { stop.disabled = false; };
    htmx.ajax('POST', reply.dataset.stopUrl, { target: reply, swap: 'outerHTML' }).then(settled, settled);
  }

  // --- Autoscroll -------------------------------------------------------------
  //
  // The reader follows the conversation while they are at the end of it: new
  // text scrolls into view. Once they scroll up to read, the view stays put and
  // "Jump to latest" appears. Sending a message always brings the view back.
  //
  // An update makes the list taller before we get to look, so "at the end" is
  // judged against the height seen last time. The live scrollTop is used rather
  // than the last scroll event, which may not have been delivered yet.

  let following = true;
  let forceFollow = false; // the reader just sent something
  let seenHeight = 0; // the list's height when we last looked

  const atEnd = (log, height) => height - log.scrollTop - log.clientHeight <= NEAR_BOTTOM_PX;

  function scrollToEnd() {
    const log = scroller();
    if (log) log.scrollTop = log.scrollHeight;
  }

  function syncJumpButton() {
    const jump = jumpButton();
    if (jump) jump.hidden = following;
  }

  // onListChanged runs after the list gains or changes content.
  function onListChanged() {
    const log = scroller();
    syncStreaming();
    decorateCode();
    following = forceFollow || atEnd(log, seenHeight);
    forceFollow = false;
    if (following) scrollToEnd();
    seenHeight = log.scrollHeight;
    syncJumpButton();
  }

  function onScroll() {
    const log = scroller();
    seenHeight = log.scrollHeight;
    following = atEnd(log, seenHeight);
    syncJumpButton();
  }

  // Earlier messages go in above what is on screen. The reader is reading the top of the list, so what they were looking at is kept where
  // it is: the list grows upward by as much as the new messages take, and the scroll position grows with it.
  let loadingEarlier = null;
  document.body.addEventListener('htmx:beforeRequest', (event) => {
    const log = scroller();
    if (!log || !(event.target instanceof Element) || !event.target.closest('#earlier')) return;
    loadingEarlier = { xhr: event.detail.xhr, height: log.scrollHeight, top: log.scrollTop };
  });
  document.body.addEventListener('htmx:afterSettle', (event) => {
    if (!loadingEarlier || event.detail.xhr !== loadingEarlier.xhr) return;
    const log = scroller();
    if (log) {
      log.scrollTop = loadingEarlier.top + (log.scrollHeight - loadingEarlier.height);
      seenHeight = log.scrollHeight;
      following = atEnd(log, seenHeight);
      syncJumpButton();
    }
    loadingEarlier = null;
  });
  document.body.addEventListener('htmx:responseError', () => { loadingEarlier = null; });

  // --- Code blocks: copy -------------------------------------------------------

  // decorateCode puts a Copy button in each code block of a finished reply. The
  // server wraps every block in div.code-block. It waits for the end because a
  // streaming reply is replaced on every update.
  function decorateCode() {
    const blocks = document.querySelectorAll(
      '#messages [data-role="assistant"]:not([data-status="pending"]):not([data-status="streaming"]) .message-body .code-block');
    for (const block of blocks) {
      if (block.querySelector('.copy-code')) continue;
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'btn btn-sm btn-outline-secondary copy-code';
      button.dataset.testid = 'copy-code';
      button.setAttribute('aria-label', 'Copy code');
      button.textContent = 'Copy';
      block.append(button);
    }
  }

  // copyText uses the Clipboard API where the page may (https, localhost) and
  // the older way where it may not.
  async function copyText(text) {
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(text);
        return true;
      }
    } catch {
      // fall through to the older way
    }
    const area = document.createElement('textarea');
    area.value = text;
    area.setAttribute('readonly', '');
    area.className = 'visually-hidden';
    document.body.append(area);
    area.select();
    let copied = false;
    try {
      copied = document.execCommand('copy');
    } catch {
      copied = false;
    }
    area.remove();
    return copied;
  }

  // copyMessage copies a reply as it is (Markdown), which its button holds, not the page's rendering of it.
  async function copyMessage(button) {
    let copied = false;
    try {
      copied = await copyText(button.dataset.raw || '');
    } catch {
      copied = false;
    }
    button.textContent = copied ? 'Copied' : 'Copy failed';
    announce(copied ? 'Reply copied.' : 'Copying failed.');
    window.setTimeout(() => { button.textContent = 'Copy'; }, 2000);
  }

  async function copyCode(button) {
    const pre = button.parentElement.querySelector('pre');
    const text = pre ? pre.textContent.replace(/\n$/, '') : '';
    const copied = await copyText(text);
    button.textContent = copied ? 'Copied' : 'Copy failed';
    announce(copied ? 'Code copied.' : 'Copying failed.');
    window.setTimeout(() => { button.textContent = 'Copy'; }, 2000);
  }

  // --- Wiring -----------------------------------------------------------------

  document.addEventListener('click', (event) => {
    if (!(event.target instanceof Element)) return;
    const copy = event.target.closest('.copy-code');
    const copyReply = event.target.closest('[data-testid="copy-message"]');
    if (copy) {
      copyCode(copy);
    } else if (copyReply) {
      copyMessage(copyReply);
    } else if (event.target.closest('#composer [data-testid="stop"]')) {
      stopStreaming();
    } else if (event.target.closest('#jump-latest')) {
      following = true;
      scrollToEnd();
      syncJumpButton();
    }
  });

  // The list the observers are watching. A boosted page swap (starting a chat)
  // brings a new one, so binding is done again then.
  let bound = null;
  let observers = [];

  function bind() {
    const log = scroller();
    if (log === bound) return;
    for (const observer of observers) observer.disconnect();
    observers = [];
    bound = log;
    if (!log) return; // not a chat page
    log.addEventListener('scroll', onScroll, { passive: true });
    const changes = new MutationObserver(onListChanged);
    changes.observe(log, {
      childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ['data-status'],
    });
    // The list's own size changes when the composer grows or the window resizes.
    const resizes = new ResizeObserver(() => { if (following) scrollToEnd(); });
    resizes.observe(log);
    observers.push(changes, resizes);
    following = true;
    forceFollow = false;
    decorateCode();
    scrollToEnd();
    seenHeight = log.scrollHeight;
    wasStreaming = false;
    syncStreaming();
    autosize();
  }

  // --- Contact picker (@) -------------------------------------------------------
  //
  // Typing @ and a few letters offers the people they may mean (the server draws the
  // list, so every text in it is escaped). Choosing one confirms it with the agent's
  // service and puts @(Name) in the message, as the agent's own page does.

  const MENTION = /(?:^|\s)@([^\s@()]*)$/;
  const PICKER_DELAY_MS = 120;
  let pickerSeq = 0; // an answer that comes after a newer question is dropped
  let pickerTimer = 0;

  const picker = () => $('#contact-picker');
  const options = () => [...document.querySelectorAll('#contact-picker .contact-option')];

  // mentionBeforeCursor is the @word the cursor is at the end of, or null.
  function mentionBeforeCursor(box) {
    const before = box.value.slice(0, box.selectionStart);
    const match = MENTION.exec(before);
    return match ? { query: match[1], start: before.length - match[1].length - 1 } : null;
  }

  function hidePicker() {
    pickerSeq += 1;
    const box = picker();
    if (box) {
      box.hidden = true;
      box.replaceChildren();
    }
  }

  async function showPicker() {
    const box = input();
    const list = picker();
    const mention = box && mentionBeforeCursor(box);
    if (!list || !mention || inFlight()) {
      hidePicker();
      return;
    }
    const seq = ++pickerSeq;
    try {
      const res = await fetch('/contacts?q=' + encodeURIComponent(mention.query), { headers: { 'HX-Request': 'true' } });
      if (!res.ok || seq !== pickerSeq) return;
      list.innerHTML = await res.text();
      list.hidden = false;
      options()[0]?.classList.add('active');
    } catch {
      hidePicker();
    }
  }

  async function pickContact(option) {
    const box = input();
    const mention = box && mentionBeforeCursor(box);
    if (!mention) {
      hidePicker();
      return;
    }
    option.disabled = true;
    try {
      const res = await fetch('/contacts/confirm', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        // The query the list was drawn for and the name the user saw, not what is typed by now: the server checks they are the same person.
        body: new URLSearchParams({
          q: picker()?.querySelector('[data-query]')?.dataset.query ?? mention.query,
          choice: option.dataset.choice,
          name: option.dataset.name ?? '',
        }),
      });
      const text = (await res.text()).trim();
      if (!res.ok) {
        option.disabled = false;
        say(text || 'The contact could not be confirmed.');
        return;
      }
      const end = box.selectionStart;
      box.value = box.value.slice(0, mention.start) + text + ' ' + box.value.slice(end);
      const caret = mention.start + text.length + 1;
      box.setSelectionRange(caret, caret);
      hidePicker();
      autosize();
      box.focus();
    } catch {
      option.disabled = false;
      say('The contact could not be confirmed.');
    }
  }

  document.addEventListener('input', (event) => {
    if (event.target !== input()) return;
    clearTimeout(pickerTimer);
    pickerTimer = setTimeout(showPicker, PICKER_DELAY_MS);
  });

  document.addEventListener('click', (event) => {
    const option = event.target instanceof Element && event.target.closest('#contact-picker .contact-option');
    if (option) pickContact(option);
  });

  // While the picker is open, the arrows move through it, Enter or Tab choose and Escape closes it. It runs before the
  // composer's own Enter, which would send the message.
  document.addEventListener('keydown', (event) => {
    const list = picker();
    if (!list || list.hidden || event.target !== input()) return;
    const items = options();
    const at = items.findIndex((el) => el.classList.contains('active'));
    const move = (to) => {
      event.preventDefault();
      items.forEach((el) => el.classList.remove('active'));
      items[(to + items.length) % items.length]?.classList.add('active');
    };
    if (event.key === 'ArrowDown') move(at + 1);
    else if (event.key === 'ArrowUp') move(at - 1);
    else if (event.key === 'Escape') {
      event.preventDefault();
      hidePicker();
    } else if ((event.key === 'Enter' || event.key === 'Tab') && !event.isComposing && items[Math.max(at, 0)]) {
      event.preventDefault();
      event.stopImmediatePropagation();
      pickContact(items[Math.max(at, 0)]);
    }
  }, true);

  document.body.addEventListener('htmx:afterSettle', bind);
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', bind);
  } else {
    bind();
  }
})();
