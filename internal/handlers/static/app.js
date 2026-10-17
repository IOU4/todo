// HTMX normally leaves 4xx/5xx responses unswapped. Our mutation endpoints
// return safe, server-rendered workspace fragments with visible error messages.
document.body.addEventListener("htmx:beforeSwap", (event) => {
  if (event.detail.xhr.getResponseHeader("X-Todo-Swap-Error") === "true") {
    event.detail.shouldSwap = true;
  }
});
