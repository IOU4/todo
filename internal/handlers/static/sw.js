// Notifications only: never cache private list pages or intercept requests.
self.addEventListener("install", (event) => {
  event.waitUntil(self.skipWaiting());
});

self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener("push", (event) => {
  let message = {};
  try {
    message = event.data ? event.data.json() : {};
  } catch (_) {
    // Still show a useful notification if a provider delivers an empty payload.
  }
  event.waitUntil(self.registration.showNotification(message.title || "Todo reminder", {
    body: message.body || "A todo item is due tomorrow.",
    tag: message.tag || "todo-reminder",
    data: { url: "/lists/todo" },
  }));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = new URL("/lists/todo", self.location.origin).href;
  event.waitUntil((async () => {
    const windows = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    for (const client of windows) {
      if (new URL(client.url).origin === self.location.origin) {
        await client.navigate(url);
        return client.focus();
      }
    }
    return self.clients.openWindow(url);
  })());
});
