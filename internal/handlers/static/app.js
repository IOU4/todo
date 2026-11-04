function localDateTime(date) {
  const pad = (value) => String(value).padStart(2, "0");
  return `${String(date.getFullYear()).padStart(4, "0")}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

function syncDueOffset(input) {
  const date = new Date(input.value);
  input.form.elements.due_offset.value = Number.isNaN(date.getTime()) ? 0 : date.getTimezoneOffset();
}

function updateCountdowns() {
  const now = Date.now();
  document.querySelectorAll("[data-countdown]").forEach((element) => {
    const due = new Date(element.dateTime);
    if (Number.isNaN(due.getTime())) return;
    const overdue = due.getTime() <= now;
    const minutes = Math.max(0, Math.ceil((due.getTime() - now) / 60000));
    const text = overdue
      ? "Overdue"
      : `${Math.floor(minutes / 1440)}d ${Math.floor((minutes % 1440) / 60)}h ${minutes % 60}m`;
    if (element.textContent !== text) element.textContent = text;
    element.classList.toggle("is-overdue", overdue);
    element.title = `Due ${due.toLocaleString()}`;
  });
}

function initializeDueDates() {
  document.querySelectorAll('input[name="due_at"]').forEach((input) => {
    if (input.dataset.dueAt) {
      const due = new Date(input.dataset.dueAt);
      if (!Number.isNaN(due.getTime())) input.value = localDateTime(due);
      delete input.dataset.dueAt;
    }
    const zoneLabel = input.closest(".due-field")?.querySelector("[data-due-zone]");
    if (zoneLabel) zoneLabel.textContent = "(your local time, optional)";
    input.setAttribute("aria-label", "Due date and time (your local time, optional)");
    input.title = "Due date and time (your local time, optional)";
    syncDueOffset(input);
  });
  updateCountdowns();
}

// Capture before HTMX (or a regular form submission) serializes the fields.
document.addEventListener("submit", (event) => {
  const input = event.target.querySelector('input[name="due_at"]');
  if (input) syncDueOffset(input);
}, true);

document.body.addEventListener("htmx:afterSwap", () => {
  initializeDueDates();
  initializeNotifications();
});

// HTMX normally leaves 4xx/5xx responses unswapped. Our mutation endpoints
// return safe, server-rendered workspace fragments with visible error messages.
document.body.addEventListener("htmx:beforeSwap", (event) => {
  if (event.detail.xhr.getResponseHeader("X-Todo-Swap-Error") === "true") {
    event.detail.shouldSwap = true;
  }
});

let notificationWorker;

function getNotificationWorker() {
  if (!notificationWorker) {
    notificationWorker = navigator.serviceWorker.register("/sw.js", {
      scope: "/", updateViaCache: "none",
    }).then(() => navigator.serviceWorker.ready).catch((error) => {
      notificationWorker = undefined;
      throw error;
    });
  }
  return notificationWorker;
}

function vapidKeyBytes(raw) {
  const padded = raw.replace(/-/g, "+").replace(/_/g, "/") + "=".repeat((4 - raw.length % 4) % 4);
  return Uint8Array.from(atob(padded), (character) => character.charCodeAt(0));
}

async function saveNotificationSetting(action, body) {
  const response = await fetch(`/notifications/${action}`, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!response.ok) throw new Error("Notification settings could not be saved. Please try again.");
}

async function initializeNotifications() {
  const panel = document.querySelector("[data-notifications]");
  if (!panel || panel.dataset.initialized) return;
  panel.dataset.initialized = "true";
  const button = panel.querySelector("[data-notification-toggle]");
  const status = panel.querySelector("[data-notification-status]");
  const schedule = "23:00 UTC the day before the UTC due date; todos added after that time are skipped.";
  if (!window.isSecureContext) {
    status.textContent = "Notifications require HTTPS (or localhost).";
    return;
  }
  if (!("Notification" in window) || !("serviceWorker" in navigator) || !("PushManager" in window)) {
    status.textContent = "Web push is unavailable in this browser. On iPhone/iPad, add the app to your Home Screen and open it there.";
    return;
  }
  if (Notification.permission === "denied") {
    status.textContent = "Notifications are blocked. Allow them in your browser's site settings to enable reminders.";
    return;
  }

  let subscription;
  const showSetting = () => {
    button.textContent = subscription ? "Disable reminders on this browser" : "Enable reminders";
    button.disabled = false;
    status.textContent = subscription ? `Reminders enabled: ${schedule}` : `Reminders: ${schedule}`;
  };
  // Install the click handler before loading the worker so setup can be retried
  // after a transient registration/network error without reloading the page.
  button.addEventListener("click", async () => {
    button.disabled = true;
    try {
      if (!subscription) {
        // Request permission directly from a user gesture, never on page load.
        const permission = await Notification.requestPermission();
        if (permission !== "granted") {
          status.textContent = "Notifications were not allowed. You can change this in your browser's site settings.";
          button.disabled = permission === "denied";
          return;
        }
      }
      const registration = await getNotificationWorker();
      subscription = await registration.pushManager.getSubscription();
      if (subscription && button.dataset.enabled === "true") {
        await saveNotificationSetting("unsubscribe", { endpoint: subscription.endpoint });
        await subscription.unsubscribe();
        subscription = null;
      } else {
        if (!subscription) {
          subscription = await registration.pushManager.subscribe({
            userVisibleOnly: true,
            applicationServerKey: vapidKeyBytes(panel.dataset.vapidPublicKey),
          });
        }
        await saveNotificationSetting("subscribe", subscription);
      }
      button.dataset.enabled = String(Boolean(subscription));
      showSetting();
    } catch (error) {
      status.textContent = error.message || "Notifications could not be enabled. Please try again.";
      button.disabled = false;
    }
  });
  try {
    const registration = await getNotificationWorker();
    subscription = await registration.pushManager.getSubscription();
    // Restore server registration after a database restore or deployment, but
    // never subscribe or prompt for permission without clicking Enable.
    if (subscription && Notification.permission === "granted") {
      await saveNotificationSetting("subscribe", subscription);
    }
    button.dataset.enabled = String(Boolean(subscription));
    showSetting();
  } catch (error) {
    status.textContent = error.message || "Notification settings could not be loaded. Please try again.";
    button.disabled = false;
  }
}

initializeDueDates();
initializeNotifications();
setInterval(updateCountdowns, 1000);
