const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");
const appJS = fs.readFileSync(path.join(__dirname, "../static/app.js"), "utf8");
const workerJS = fs.readFileSync(path.join(__dirname, "../static/sw.js"), "utf8");
const flush = () => new Promise((resolve) => setImmediate(resolve));

async function browser({ existing = false, failSave = false, secure = true, denied = false } = {}) {
  const calls = { permissions: 0, subscriptions: 0, fetches: [], unsubscriptions: 0 };
  const button = { dataset: {}, disabled: true, addEventListener: (_, listener) => { button.click = listener; } };
  const status = {};
  const panel = {
    dataset: { vapidPublicKey: "AQID" },
    querySelector: (selector) => selector === "[data-notification-toggle]" ? button : status,
  };
  let subscription = null;
  const makeSubscription = () => ({
    endpoint: "https://fcm.googleapis.com/token",
    toJSON: () => ({ endpoint: "https://fcm.googleapis.com/token", keys: { p256dh: "public", auth: "auth" } }),
    unsubscribe: async () => { calls.unsubscriptions++; subscription = null; return true; },
  });
  if (existing) subscription = makeSubscription();
  const registration = { pushManager: {
    getSubscription: async () => subscription,
    subscribe: async (options) => {
      calls.subscriptions++;
      assert.equal(options.userVisibleOnly, true);
      assert.deepEqual(Array.from(options.applicationServerKey), [1, 2, 3]);
      subscription = makeSubscription();
      return subscription;
    },
  } };
  const Notification = {
    permission: denied ? "denied" : existing ? "granted" : "default",
    requestPermission: async () => { calls.permissions++; Notification.permission = "granted"; return "granted"; },
  };
  let failing = failSave;
  const context = {
    document: {
      querySelector: () => panel,
      querySelectorAll: () => [],
      addEventListener: () => {},
      body: { addEventListener: () => {} },
    },
    navigator: { serviceWorker: { register: async (url, options) => {
      assert.equal(url, "/sw.js");
      assert.equal(options.scope, "/");
      return registration;
    }, ready: Promise.resolve(registration) } },
    window: { isSecureContext: secure, Notification, PushManager: function () {} },
    Notification, Uint8Array, atob,
    fetch: async (url, options) => {
      calls.fetches.push({ url, options });
      if (failing) { failing = false; return { ok: false }; }
      return { ok: true };
    },
    setInterval: () => {},
  };
  vm.runInNewContext(appJS, context);
  await flush();
  return { calls, button, status, context };
}

test("permission is requested only on opt-in; disabling removes the subscription", async () => {
  const { calls, button, status } = await browser();
  assert.equal(calls.permissions, 0);
  assert.equal(calls.subscriptions, 0);
  assert.equal(button.textContent, "Enable reminders");
  await button.click();
  assert.equal(calls.permissions, 1);
  assert.equal(calls.subscriptions, 1);
  assert.equal(calls.fetches[0].url, "/notifications/subscribe");
  assert.match(status.textContent, /23:00 UTC/);
  assert.match(status.textContent, /added after that time are skipped/);
  await button.click();
  assert.equal(calls.unsubscriptions, 1);
  assert.equal(calls.fetches[1].url, "/notifications/unsubscribe");
  assert.equal(button.textContent, "Enable reminders");
});

test("an existing opted-in subscription is restored without a permission prompt", async () => {
  const { calls, button } = await browser({ existing: true });
  assert.equal(calls.permissions, 0);
  assert.equal(calls.subscriptions, 0);
  assert.equal(calls.fetches.length, 1);
  assert.match(button.textContent, /Disable/);
});

test("a failed subscription save can be retried without another browser subscription", async () => {
  const { calls, button, status } = await browser({ failSave: true });
  await button.click();
  assert.match(status.textContent, /could not be saved/);
  assert.equal(button.disabled, false);
  await button.click();
  assert.equal(calls.subscriptions, 1);
  assert.equal(calls.fetches.length, 2);
  assert.match(button.textContent, /Disable/);
});

test("insecure origins and denied permission do not subscribe or prompt", async () => {
  for (const options of [{ secure: false }, { denied: true }]) {
    const { calls, button, status } = await browser(options);
    assert.equal(button.disabled, true);
    assert.equal(calls.permissions, 0);
    assert.equal(calls.subscriptions, 0);
    assert.match(status.textContent, /HTTPS|blocked/);
  }
});

test("worker displays notifications and focuses the todo list on click", async () => {
  const listeners = {};
  const shown = [];
  const navigated = [];
  let focused = 0;
  const self = {
    addEventListener: (name, listener) => { listeners[name] = listener; },
    location: { origin: "https://lists.test" },
    registration: { showNotification: async (title, options) => { shown.push({ title, options }); } },
    clients: {
      matchAll: async () => [{ url: "https://lists.test/lists/shopping", navigate: async (url) => navigated.push(url), focus: async () => { focused++; } }],
      openWindow: async () => { assert.fail("should focus the existing app window"); },
    },
  };
  vm.runInNewContext(workerJS, { self, URL });
  let pending;
  listeners.push({ data: { json: () => ({ title: "Due tomorrow", body: "Finish project", tag: "todo-42" }) }, waitUntil: (promise) => { pending = promise; } });
  await pending;
  assert.equal(shown[0].title, "Due tomorrow");
  assert.equal(shown[0].options.body, "Finish project");
  assert.equal(shown[0].options.tag, "todo-42");
  let closed = false;
  listeners.notificationclick({ notification: { close: () => { closed = true; } }, waitUntil: (promise) => { pending = promise; } });
  await pending;
  assert.equal(closed, true);
  assert.deepEqual(navigated, ["https://lists.test/lists/todo"]);
  assert.equal(focused, 1);
  listeners.push({ data: { json: () => { throw new Error("bad payload"); } }, waitUntil: (promise) => { pending = promise; } });
  await pending;
  assert.equal(shown[1].title, "Todo reminder");
});
