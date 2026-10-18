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

document.body.addEventListener("htmx:afterSwap", initializeDueDates);

// HTMX normally leaves 4xx/5xx responses unswapped. Our mutation endpoints
// return safe, server-rendered workspace fragments with visible error messages.
document.body.addEventListener("htmx:beforeSwap", (event) => {
  if (event.detail.xhr.getResponseHeader("X-Todo-Swap-Error") === "true") {
    event.detail.shouldSwap = true;
  }
});

initializeDueDates();
setInterval(updateCountdowns, 1000);
